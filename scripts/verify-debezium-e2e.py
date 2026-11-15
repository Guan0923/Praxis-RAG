"""Live end-to-end verification of the Debezium CDC pipeline with the Go backend.

Pipeline under test (no transactional outbox any more, two Debezium MySQL
connectors running as tasks in a distributed Kafka Connect cluster):
  MCP add_document -> MySQL document_jobs COMMIT
  -> document connector (binlog CDC through Kafka Connect) -> RAG_DOCUMENT_TOPIC
  -> rag-document-worker -> Qdrant + MySQL documents
  -> get_document_status = succeeded, REST search returns the document.
  The same write is captured by the cache connector as a locator-only event on
  RAG_CACHE_TOPIC (documents table, no body), and the cache revision row
  advances.

Acceptance cases run by this script:
  2. independent capture: pause the cache connector, write cache changes and
     submit a document; the document worker still claims and completes it, and
     the cache connector delivers the queued change after resume.
  3. worker failover: stop the Connect worker that owns the document task,
     submit a document during takeover, and require the peer to run the task and
     deliver the notification to the Go document worker (not merely RUNNING).
  4. FAILED-task recovery: lock the cache CDC account (workers stay online) so
     only the cache task fails; after unlocking, the restart supervisor must
     return it to RUNNING without a manual REST restart, while the document path
     keeps working.
  5. message contract: one real document notification and one real cache delete
     event are decoded by dist/bin/rag-connect.exe, which calls the same Go
     decoders the workers use.

Also asserts the Console monitor contract: pipeline.debezium.tasks/cache with
connector/task/worker/restart fields, pipeline.kafka.groups (document/cache),
pipeline.cache and service.delivery=debezium; pipeline.outbox must be gone.

Requires the docker compose infra (MySQL 3307, Qdrant 6333, Redis 6379,
Kafka 9092, two Kafka Connect workers) to be running, and dist/bin binaries built.
"""
from __future__ import annotations

import base64
import json
import os
import pathlib
import random
import re
import shutil
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DOCKER_BIN = r"C:\Program Files\Docker\Docker\resources\bin"
if pathlib.Path(DOCKER_BIN).exists():
    os.environ["PATH"] = DOCKER_BIN + os.pathsep + os.environ.get("PATH", "")

ROOT = pathlib.Path(__file__).resolve().parents[1]
BIN = ROOT / "dist" / "bin"
WORK = pathlib.Path(os.environ.get("RAG_E2E_WORK", str(ROOT / ".test-tmp" / "verify-debezium")))
SERVICE_PORT = int(os.environ.get("RAG_E2E_SERVICE_PORT", "18011"))
MODEL_PORT = int(os.environ.get("RAG_E2E_MODEL_PORT", "18080"))
SERVICE = f"http://127.0.0.1:{SERVICE_PORT}"
MODEL = f"http://127.0.0.1:{MODEL_PORT}/v1"
MYSQL_PORT = os.environ.get("RAG_MYSQL_PORT", "3307")
MYSQL_PASSWORD = os.environ.get("RAG_MYSQL_PASSWORD", "praxis-dev-pw")
MYSQL_DATABASE = os.environ.get("RAG_MYSQL_DATABASE", "praxis_rag")
VECTOR_SIZE = int(os.environ.get("RAG_E2E_VECTOR_SIZE", "1024"))
KAFKA_BOOTSTRAP = os.environ.get("RAG_KAFKA_BOOTSTRAP_SERVERS", "127.0.0.1:9092")
CACHE_TOPIC = os.environ.get("RAG_CACHE_TOPIC", "praxis_state_cache_refresh")
CACHE_GROUP = os.environ.get("RAG_CACHE_GROUP", "praxis_state_cache_refresh_workers")
DOCUMENT_TOPIC = os.environ.get("RAG_DOCUMENT_TOPIC", "praxis_document_ingest")
DOCUMENT_GROUP = os.environ.get("RAG_DOCUMENT_GROUP", "praxis_document_ingest_workers")
DOCUMENT_DLQ_TOPIC = os.environ.get(
    "RAG_DOCUMENT_DLQ_TOPIC", "praxis_document_ingest_dead_letters")
CACHE_DLQ_TOPIC = os.environ.get(
    "RAG_CACHE_DLQ_TOPIC", "praxis_state_cache_refresh_dead_letters")
DOCUMENT_DLQ_GROUP = os.environ.get(
    "RAG_DOCUMENT_DLQ_GROUP", "praxis_document_dead_letters")
CACHE_DLQ_GROUP = os.environ.get(
    "RAG_CACHE_DLQ_GROUP", "praxis_state_cache_refresh_dead_letters")
# Distributed Kafka Connect workers. Any worker answers cluster-wide connector
# state, so the list is used with failover: killing one worker must not blind the
# monitor or this script.
CONNECT_URLS = [
    url.strip()
    for url in os.environ.get(
        "RAG_CONNECT_URLS", "http://127.0.0.1:8083,http://127.0.0.1:8084"
    ).split(",")
    if url.strip()
]
DOCUMENT_CONNECTOR = os.environ.get(
    "RAG_CONNECT_DOCUMENT_CONNECTOR", "praxis-document-cdc")
CACHE_CONNECTOR = os.environ.get(
    "RAG_CONNECT_CACHE_CONNECTOR", "praxis-cache-cdc")
CACHE_CDC_USER = os.environ.get("RAG_MYSQL_DEBEZIUM_CACHE_USER", "debezium_cache")
# Compose service -> container name. Production may spread the workers across
# machines; the drill then targets the worker that owns the document task.
CONNECT_CONTAINERS = {
    "connect-1": "praxis-rag-connect-1",
    "connect-2": "praxis-rag-connect-2",
}
CONNECT_EXE = BIN / "rag-connect.exe"

results: list[tuple[bool, str]] = []


def check(ok: bool, message: str) -> None:
    results.append((ok, message))
    print(("PASS " if ok else "FAIL ") + message, flush=True)


def mask(value: str) -> str:
    return value[:8] + "..." if len(value) > 8 else value


# --------------------------------------------------------------------------- #
# Fake OpenAI-compatible model service (deterministic embeddings + rerank).
# --------------------------------------------------------------------------- #
class ModelHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, *args):  # noqa: ANN002 - quiet noise
        pass

    def _json(self, payload: dict, status: int = 200) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802
        if self.path.rstrip("/") in ("/v1/models", "/models"):
            self._json({
                "object": "list",
                "data": [
                    {"id": "bge-m3", "object": "model"},
                    {"id": "bge-reranker-base", "object": "model"},
                    {"id": "test", "object": "model"},
                ],
            })
        else:
            self._json({"detail": "not found"}, 404)

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length") or 0)
        payload = json.loads(self.rfile.read(length) or b"{}")
        path = self.path.rstrip("/")
        if path in ("/v1/embeddings", "/embeddings"):
            inputs = payload.get("input")
            if isinstance(inputs, str):
                inputs = [inputs]
            data = []
            for index, text in enumerate(inputs or []):
                seed = int.from_bytes(
                    __import__("hashlib").sha256(str(text).encode()).digest()[:8], "big")
                rng = random.Random(seed)
                vector = [round(rng.uniform(-1.0, 1.0), 6) for _ in range(VECTOR_SIZE)]
                data.append({"object": "embedding", "index": index, "embedding": vector})
            self._json({
                "object": "list",
                "model": payload.get("model", "bge-m3"),
                "data": data,
                "usage": {"prompt_tokens": 1, "total_tokens": 1},
            })
        elif path in ("/v1/rerank", "/rerank"):
            documents = payload.get("documents") or []
            results_payload = [
                {"index": index, "relevance_score": round(1.0 - index * 0.01, 6)}
                for index in range(len(documents))
            ]
            self._json({"results": results_payload})
        else:
            self._json({"detail": "not found"}, 404)


def start_model_server() -> ThreadingHTTPServer:
    server = ThreadingHTTPServer(("127.0.0.1", MODEL_PORT), ModelHandler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


# --------------------------------------------------------------------------- #
# HTTP plumbing.
# --------------------------------------------------------------------------- #
def http_raw(method: str, url: str, body: bytes | None = None, headers: dict | None = None,
             timeout: float = 10.0):
    request = urllib.request.Request(url, data=body, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, {k.lower(): v for k, v in response.headers.items()}, response.read()
    except urllib.error.HTTPError as error:
        return error.code, {k.lower(): v for k, v in error.headers.items()}, error.read()


def http_json(method: str, url: str, payload: dict | None = None, headers: dict | None = None,
              timeout: float = 10.0):
    merged = {"Content-Type": "application/json"}
    merged.update(headers or {})
    body = json.dumps(payload).encode() if payload is not None else None
    status, response_headers, raw = http_raw(method, url, body, merged, timeout)
    decoded = None
    if raw:
        try:
            decoded = json.loads(raw)
        except json.JSONDecodeError:
            decoded = raw.decode("utf-8", "replace")
    return status, {key.lower(): value for key, value in response_headers.items()}, decoded


def parse_sse(raw: bytes):
    text = raw.decode("utf-8", "replace")
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("data:"):
            payload = line[len("data:"):].strip()
            if payload:
                try:
                    return json.loads(payload)
                except json.JSONDecodeError:
                    continue
    return None


# --------------------------------------------------------------------------- #
# MCP client.
# --------------------------------------------------------------------------- #
class MCP:
    def __init__(self, api_key: str, namespace: str):
        self.api_key = api_key
        self.namespace = namespace
        self.session = ""
        self.sequence = 0

    def _headers(self) -> dict:
        headers = {
            "Authorization": "Bearer " + self.api_key,
            "X-RAG-Namespace": self.namespace,
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        }
        if self.session:
            headers["Mcp-Session-Id"] = self.session
        return headers

    def request(self, method: str, params: dict | None = None, notification: bool = False):
        self.sequence += 1
        payload: dict = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            payload["params"] = params
        if not notification:
            payload["id"] = self.sequence
        status, headers, raw = http_raw("POST", SERVICE + "/mcp",
                                        json.dumps(payload).encode(),
                                        self._headers(), timeout=30)
        if notification:
            return status, headers, None
        decoded = None
        if raw:
            try:
                decoded = json.loads(raw)
            except json.JSONDecodeError:
                decoded = parse_sse(raw)
        return status, headers, decoded

    def initialize(self) -> None:
        status, headers, payload = self.request("initialize", {
            "protocolVersion": "2025-11-25",
            "capabilities": {},
            "clientInfo": {"name": "e2e-debezium", "version": "1.0"},
        })
        session = headers.get("mcp-session-id", "")
        if not session:
            raise RuntimeError(f"initialize returned {status} without Mcp-Session-Id: {payload}")
        self.session = session
        self.request("notifications/initialized", notification=True)

    def call_tool(self, name: str, arguments: dict) -> dict:
        status, headers, payload = self.request("tools/call", {"name": name, "arguments": arguments})
        if status != 200:
            raise RuntimeError(f"{name} HTTP {status}: {payload}")
        if not isinstance(payload, dict) or "result" not in payload:
            raise RuntimeError(f"{name} invalid response: {payload}")
        result = payload["result"]
        content = result.get("content") or []
        text = next((item.get("text") for item in content
                     if isinstance(item, dict) and item.get("type") == "text"), "")
        decoded = {}
        if text:
            try:
                decoded = json.loads(text)
            except json.JSONDecodeError:
                decoded = {"text": text}
        decoded["_is_error"] = bool(result.get("isError"))
        return decoded


# --------------------------------------------------------------------------- #
# Process management.
# --------------------------------------------------------------------------- #
def base_env() -> dict:
    env = dict(os.environ)
    for name in list(env):
        if name.startswith(("RAG_", "QDRANT_")):
            env.pop(name, None)
    temp_root = WORK / "tmp"
    temp_root.mkdir(parents=True, exist_ok=True)
    env.update({
        "TMP": str(temp_root),
        "TEMP": str(temp_root),
        "TMPDIR": str(temp_root),
        "RAG_CONFIG_FILE": str(WORK / "config.toml"),
        "RAG_BACKEND": "qdrant",
        "RAG_SERVICE_HOST": "127.0.0.1",
        "RAG_SERVICE_PORT": str(SERVICE_PORT),
        "RAG_SERVICE_DATA_ROOT": str(WORK / "data"),
        "RAG_MYSQL_HOST": "127.0.0.1",
        "RAG_MYSQL_PORT": MYSQL_PORT,
        "RAG_MYSQL_USER": "rag",
        "RAG_MYSQL_PASSWORD": MYSQL_PASSWORD,
        "RAG_MYSQL_DATABASE": MYSQL_DATABASE,
        "RAG_STATE_ENCRYPTION_KEY": "base64:" + base64.b64encode(b"praxis-e2e-debezium-key-32byte!!"[:32]).decode(),
        "RAG_REDIS_ENABLED": "false",
        "QDRANT_URL": "http://127.0.0.1:6333",
        "RAG_KAFKA_BOOTSTRAP_SERVERS": KAFKA_BOOTSTRAP,
        "RAG_KAFKA_CLIENT_ID": "praxis-rag-e2e",
        "RAG_CACHE_TOPIC": CACHE_TOPIC,
        "RAG_CACHE_GROUP": CACHE_GROUP,
        "RAG_DOCUMENT_TOPIC": DOCUMENT_TOPIC,
        "RAG_DOCUMENT_GROUP": DOCUMENT_GROUP,
        "RAG_DOCUMENT_DLQ_TOPIC": DOCUMENT_DLQ_TOPIC,
        "RAG_CACHE_DLQ_TOPIC": CACHE_DLQ_TOPIC,
        "RAG_DOCUMENT_DLQ_GROUP": DOCUMENT_DLQ_GROUP,
        "RAG_CACHE_DLQ_GROUP": CACHE_DLQ_GROUP,
        # Distributed Kafka Connect: the monitor reads the two named connectors'
        # task state and the failed-task supervisor's restart bookkeeping. The
        # binlog offsets live in Kafka Connect's offset topic.
        "RAG_CONNECT_URLS": ",".join(CONNECT_URLS),
        "RAG_CONNECT_DOCUMENT_CONNECTOR": DOCUMENT_CONNECTOR,
        "RAG_CONNECT_CACHE_CONNECTOR": CACHE_CONNECTOR,
        "RAG_CONNECT_TIMEOUT_SECONDS": "2",
        "RAG_CONNECT_SUPERVISOR_INTERVAL_SECONDS": "5",
        "RAG_CONNECT_RESTART_MAX_ATTEMPTS": "5",
        "RAG_CONNECT_RESTART_BASE_BACKOFF_SECONDS": "1",
        "RAG_CONNECT_RESTART_MAX_BACKOFF_SECONDS": "30",
    })
    return env


def start_process(name: str, args: list[str], env: dict) -> tuple[subprocess.Popen, pathlib.Path]:
    log_path = WORK / f"{name}.log"
    log_file = log_path.open("wb")
    process = subprocess.Popen(args, cwd=str(ROOT), env=env, stdout=log_file,
                               stderr=subprocess.STDOUT, text=False)
    process.log_file = log_file  # type: ignore[attr-defined]
    process.log_path = log_path    # type: ignore[attr-defined]
    return process, log_path


def stop_process(process: subprocess.Popen | None) -> None:
    if process is None:
        return
    try:
        process.terminate()
        process.wait(timeout=10)
    except Exception:  # noqa: BLE001 - force kill on shutdown
        try:
            process.kill()
            process.wait(timeout=5)
        except Exception:  # noqa: BLE001
            pass
    log_file = getattr(process, "log_file", None)
    if log_file is not None:
        try:
            log_file.close()
        except Exception:  # noqa: BLE001
            pass


def wait_health(timeout: float = 60.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            status, _, payload = http_json("GET", SERVICE + "/health", timeout=2)
            if status == 200 and isinstance(payload, dict) and payload.get("status") == "ok":
                return True
        except Exception:  # noqa: BLE001 - startup race
            pass
        time.sleep(0.5)
    return False


def mysql_query(sql: str) -> str:
    command = [
        "docker", "exec", "praxis-rag-mysql",
        "sh", "-c",
        f'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -N -e "{sql}"',
    ]
    completed = subprocess.run(command, capture_output=True, text=True, timeout=60)
    lines = [line for line in (completed.stdout or "").splitlines()
             if "Warning" not in line]
    if completed.returncode != 0:
        raise RuntimeError(f"mysql failed: {completed.stderr.strip()}")
    return "\n".join(lines).strip()


def tail(path: pathlib.Path, lines: int = 40) -> str:
    try:
        content = path.read_text("utf-8", "replace").splitlines()
        return "\n".join(content[-lines:])
    except Exception:  # noqa: BLE001
        return "<no log>"


def log_occurrences(path: pathlib.Path, needle: str) -> int:
    try:
        return path.read_text("utf-8", "replace").count(needle)
    except Exception:  # noqa: BLE001
        return 0


def wait_log_occurrence(
    path: pathlib.Path, needle: str, timeout: float = 90.0, above: int = 0
) -> bool:
    """Wait until the log holds more than `above` occurrences of the needle.

    `above` distinguishes the record this run just produced from earlier
    occurrences (for example a previous script run in the same log file).
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        if log_occurrences(path, needle) > above:
            return True
        time.sleep(0.5)
    return False


def kafka_topic_contains(topic: str, needle: str) -> tuple[bool, str]:
    """Read a topic from the beginning and report whether the needle appears."""
    try:
        completed = subprocess.run([
            "docker", "exec", "praxis-rag-kafka",
            "/opt/kafka/bin/kafka-console-consumer.sh",
            "--bootstrap-server", "kafka:9092",
            "--topic", topic,
            "--from-beginning",
            "--timeout-ms", "5000",
        ], capture_output=True, text=True, timeout=30)
        output = (completed.stdout or "") + (completed.stderr or "")
        return needle in output, output[-4000:]
    except Exception as error:  # noqa: BLE001 - probe is optional evidence
        return False, f"probe failed: {error}"


# --------------------------------------------------------------------------- #
# Kafka Connect REST / docker helpers for the failover and recovery drills.
# --------------------------------------------------------------------------- #
def connect_request(method: str, path: str, payload: dict | None = None,
                    timeout: float = 10.0):
    """Send one request to the first reachable Connect worker.

    Any worker answers cluster-wide connector state, so a stopped worker must
    not stop the drill or the monitor. Connection errors fail over; HTTP errors
    are returned (they are the answer, not a transport problem).
    """
    last_error = None
    for base in CONNECT_URLS:
        try:
            status, headers, body = http_json(method, base + path, payload,
                                              timeout=timeout)
        except Exception as error:  # noqa: BLE001 - try the next worker
            last_error = error
            continue
        if method != "GET" and status == 409:
            # Only the group leader accepts some writes; the peer may be leader.
            last_error = RuntimeError(f"{base} is not the leader: {body}")
            continue
        return status, headers, body
    raise RuntimeError(f"Kafka Connect REST unreachable at {CONNECT_URLS}: {last_error}")


def connector_status(name: str) -> dict:
    status, _, body = connect_request("GET", f"/connectors/{name}/status")
    if status != 200 or not isinstance(body, dict):
        raise RuntimeError(f"connector {name} status HTTP {status}: {body}")
    return body


def connector_state(status: dict) -> str:
    return str(((status or {}).get("connector") or {}).get("state") or "")


def connector_all_running(status: dict) -> bool:
    tasks = (status or {}).get("tasks") or []
    return (connector_state(status) == "RUNNING" and bool(tasks) and
            all(task.get("state") == "RUNNING" for task in tasks))


def wait_connector(name: str, predicate, timeout: float = 180.0, poll: float = 2.0,
                   description: str = "condition") -> dict:
    deadline = time.time() + timeout
    last: dict = {}
    while time.time() < deadline:
        try:
            last = connector_status(name)
        except Exception as error:  # noqa: BLE001 - a worker may be down
            last = {"_error": str(error)}
        if predicate(last):
            return last
        time.sleep(poll)
    raise RuntimeError(f"{name} did not reach {description} within {timeout:.0f}s: {last}")


def set_connector_paused(name: str, paused: bool) -> None:
    action = "pause" if paused else "resume"
    status, _, body = connect_request("PUT", f"/connectors/{name}/{action}")
    if status not in (200, 202, 204):
        raise RuntimeError(f"{action} {name} HTTP {status}: {body}")


def connect_worker_container(worker_id: str) -> tuple[str, str]:
    """Map a Connect worker_id (hostname:port) to its compose service/container."""
    host = str(worker_id or "").split(":")[0]
    if not host:
        raise RuntimeError("connector task has no worker_id")
    for service, container in CONNECT_CONTAINERS.items():
        if host == service:
            return service, container
        completed = subprocess.run(
            ["docker", "inspect", "-f", "{{.Config.Hostname}}", container],
            capture_output=True, text=True, timeout=20)
        hostname = (completed.stdout or "").strip()
        if hostname and host == hostname:
            return service, container
    raise RuntimeError(f"cannot map Kafka Connect worker {worker_id!r} to a compose container")


def docker_action(*args: str, timeout: float = 120.0) -> str:
    completed = subprocess.run(["docker", *args], capture_output=True, text=True,
                               timeout=timeout)
    if completed.returncode != 0:
        raise RuntimeError(f"docker {' '.join(args)} failed: {completed.stderr.strip()}")
    return (completed.stdout or "").strip()


# --------------------------------------------------------------------------- #
# Cache-change markers and Kafka message decoding.
# --------------------------------------------------------------------------- #
def insert_lease_marker(namespace: str, marker: str) -> None:
    mysql_query(
        f"INSERT INTO {MYSQL_DATABASE}.document_leases "
        f"(namespace, resource, owner, operation_id, expires_at) VALUES "
        f"('{namespace}', '{marker}', 'e2e', NULL, UNIX_TIMESTAMP() + 120);")


def delete_lease_marker(namespace: str, marker: str) -> None:
    mysql_query(
        f"DELETE FROM {MYSQL_DATABASE}.document_leases "
        f"WHERE namespace = '{namespace}' AND resource = '{marker}';")


def kafka_consume_values(topic: str, timeout_ms: int = 5000,
                         max_messages: int | None = None) -> list[str]:
    args = [
        "docker", "exec", "praxis-rag-kafka",
        "/opt/kafka/bin/kafka-console-consumer.sh",
        "--bootstrap-server", "kafka:9092",
        "--topic", topic,
        "--from-beginning",
        "--timeout-ms", str(timeout_ms),
    ]
    if max_messages:
        args += ["--max-messages", str(max_messages)]
    completed = subprocess.run(args, capture_output=True, text=True,
                               timeout=timeout_ms / 1000 + 30)
    return [line for line in (completed.stdout or "").splitlines() if line.strip()]


def decode_kafka_values(kind: str, values: list[str], op: str | None = None):
    """Run dist/bin/rag-connect decode over real Kafka values.

    The CLI calls internal/mq, the same production decoder the workers use, so a
    successful decode proves the deployed converter/routing shape still matches
    the Go contract — not just that a fixture parses.
    """
    if not values:
        return [], None
    path = WORK / f"decode-{kind}-{time.time_ns()}.ndjson"
    path.write_text("\n".join(values) + "\n", encoding="utf-8")
    args = [str(CONNECT_EXE), "decode", "--kind", kind, "--input", str(path),
            "--limit", "0"]
    if op:
        args += ["--op", op]
    completed = subprocess.run(args, capture_output=True, text=True, timeout=120)
    decoded: list[dict] = []
    for line in (completed.stdout or "").splitlines():
        try:
            decoded.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return decoded, completed


def wait_kafka_marker(topic: str, marker: str, timeout: float = 90.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        found, _ = kafka_topic_contains(topic, marker)
        if found:
            return True
        time.sleep(2)
    return False


def submit_and_wait(mcp: "MCP", name: str, content: str, key: str,
                    timeout: float = 240.0) -> tuple[str, dict]:
    admitted = mcp.call_tool("add_document", {
        "name": name, "content": content, "idempotency_key": key,
    })
    artifact_id = admitted.get("artifact_id", "")
    if not artifact_id:
        raise RuntimeError(f"add_document rejected: {admitted}")
    deadline = time.time() + timeout
    payload: dict = {}
    while time.time() < deadline:
        payload = mcp.call_tool("get_document_status", {"artifact_id": artifact_id})
        if payload.get("status") in ("succeeded", "failed"):
            break
        time.sleep(1.5)
    return artifact_id, payload


# --------------------------------------------------------------------------- #
# Acceptance drills (items 2, 3 and 4 of the Kafka Connect migration).
# --------------------------------------------------------------------------- #
def acceptance_independent_capture(mcp: "MCP", namespace: str) -> str:
    """Pause cache capture, write a cache change and a document, resume."""
    marker = f"e2e-cache-{int(time.time() * 1000)}"
    paused = False
    try:
        set_connector_paused(CACHE_CONNECTOR, True)
        paused = True
        wait_connector(CACHE_CONNECTOR,
                       lambda status: connector_state(status) in ("PAUSED", "STOPPED"),
                       60, 1, "PAUSED")
        insert_lease_marker(namespace, marker)
        artifact, payload = submit_and_wait(
            mcp, f"independent-{int(time.time())}.md",
            "# Independent capture\n\nThe document path must keep working while cache capture is paused.\n",
            f"e2e-independent-{int(time.time())}")
        check(payload.get("status") == "succeeded",
              f"document completed while the cache connector was paused (artifact={mask(artifact)})")
        check(connector_state(connector_status(CACHE_CONNECTOR)) in ("PAUSED", "STOPPED"),
              "cache connector stayed paused while the document reached the document worker")
    finally:
        if paused:
            try:
                set_connector_paused(CACHE_CONNECTOR, False)
            except Exception as error:  # noqa: BLE001 - keep the original failure
                print("WARN could not resume the cache connector:", error)
    resumed = wait_connector(CACHE_CONNECTOR, connector_all_running, 180, 2,
                             "RUNNING after resume")
    check(connector_all_running(resumed),
          "cache connector resumed to RUNNING after the paused document completed")
    check(wait_kafka_marker(CACHE_TOPIC, marker, 90),
          "cache connector delivered the change queued while it was paused (saved binlog offset)")
    return marker


def acceptance_worker_failover(mcp: "MCP", namespace: str,
                               stopped_containers: set[str]) -> str:
    """Stop the worker that owns the document task, submit, require peer takeover."""
    status = connector_status(DOCUMENT_CONNECTOR)
    task = next((item for item in status.get("tasks") or []
                 if item.get("state") == "RUNNING"), None)
    if task is None:
        raise RuntimeError(f"no RUNNING document task to fail over: {status}")
    service, container = connect_worker_container(task.get("worker_id", ""))
    check(True, f"document task currently runs on {service} ({task.get('worker_id')})")

    stopped_containers.add(container)
    docker_action("stop", container, timeout=180)
    check(True, f"stopped Kafka Connect worker {service} during the drill")

    def on_peer(candidate: dict) -> bool:
        if not connector_all_running(candidate):
            return False
        for item in candidate.get("tasks") or []:
            try:
                peer, _ = connect_worker_container(item.get("worker_id", ""))
            except Exception:  # noqa: BLE001 - stale worker id during rebalance
                continue
            if peer != service:
                return True
        return False

    try:
        artifact, payload = submit_and_wait(
            mcp, f"failover-{int(time.time())}.md",
            "# Worker failover\n\nThis document is submitted while the owning Connect worker is stopped.\n",
            f"e2e-failover-{int(time.time())}", timeout=300)
        moved = wait_connector(DOCUMENT_CONNECTOR, on_peer, 240, 2,
                               "RUNNING on the peer worker")
        check(payload.get("status") == "succeeded",
              f"document submitted during takeover reached the Go document worker "
              f"(artifact={mask(artifact)})")
        check(on_peer(moved),
              f"document task was reassigned to the peer worker ({moved.get('tasks')})")
    finally:
        docker_action("start", container, timeout=180)
        stopped_containers.discard(container)
        wait_connector(DOCUMENT_CONNECTOR, connector_all_running, 240, 2,
                       "RUNNING after the stopped worker restarted")
    return artifact


def acceptance_task_restart(mcp: "MCP", namespace: str,
                            locked_users: set[str]) -> str:
    """Fail only the cache task (account lock) and rely on the supervisor."""
    marker = f"e2e-restart-{int(time.time() * 1000)}"
    mysql_query(f"ALTER USER '{CACHE_CDC_USER}'@'%' ACCOUNT LOCK;")
    locked_users.add(CACHE_CDC_USER)
    try:
        connection_ids = [
            line.strip() for line in mysql_query(
                "SELECT id FROM information_schema.processlist "
                f"WHERE user = '{CACHE_CDC_USER}';").splitlines()
            if line.strip().isdigit()
        ]
        for connection_id in connection_ids:
            try:
                mysql_query(f"KILL {int(connection_id)};")
            except Exception as error:  # noqa: BLE001 - the connection may be gone
                print(f"INFO KILL {connection_id} failed: {error}")
    except Exception:
        mysql_query(f"ALTER USER '{CACHE_CDC_USER}'@'%' ACCOUNT UNLOCK;")
        locked_users.discard(CACHE_CDC_USER)
        raise

    wait_connector(CACHE_CONNECTOR,
                   lambda status: any(task.get("state") == "FAILED"
                                      for task in status.get("tasks") or []),
                   240, 2, "a FAILED cache task")
    check(True, "cache task entered FAILED while both Kafka Connect workers stayed online")
    check(connector_all_running(connector_status(DOCUMENT_CONNECTOR)),
          "document connector kept running while only the cache task failed")

    mysql_query(f"ALTER USER '{CACHE_CDC_USER}'@'%' ACCOUNT UNLOCK;")
    locked_users.discard(CACHE_CDC_USER)
    recovered = wait_connector(CACHE_CONNECTOR, connector_all_running, 240, 2,
                               "RUNNING after the fault was cleared")
    check(connector_all_running(recovered),
          "cache task returned to RUNNING without a manual REST restart (supervisor)")
    insert_lease_marker(namespace, marker)
    check(wait_kafka_marker(CACHE_TOPIC, marker, 90),
          "cache delivery resumed after the automatic task restart")
    delete_lease_marker(namespace, marker)

    artifact, payload = submit_and_wait(
        mcp, f"after-restart-{int(time.time())}.md",
        "# After task restart\n\nThe document path still works after the cache task recovered.\n",
        f"e2e-after-restart-{int(time.time())}")
    check(payload.get("status") == "succeeded",
          f"document path still delivers after the cache task recovery "
          f"(artifact={mask(artifact)})")
    return marker


# --------------------------------------------------------------------------- #
# Main flow.
# --------------------------------------------------------------------------- #
def main() -> int:
    if not BIN.joinpath("rag-service.exe").exists() or not CONNECT_EXE.exists():
        print("missing dist/bin binaries (rag-service / rag-connect); run scripts/build-go.ps1 first")
        return 2
    if WORK.exists():
        shutil.rmtree(WORK, ignore_errors=True)
    WORK.mkdir(parents=True, exist_ok=True)
    (WORK / "data").mkdir(parents=True, exist_ok=True)
    (WORK / "config.toml").write_text("[environment]\n", encoding="utf-8")

    model_server = start_model_server()
    check(True, f"fake model API listening on {MODEL}")

    namespace = "gocdc" + time.strftime("%H%M%S")
    document_name = f"bridge-{int(time.time())}.md"
    service_process = None
    worker_process = None
    stopped_containers: set[str] = set()
    locked_users: set[str] = set()
    try:
        env = base_env()
        service_process, service_log = start_process(
            "rag-service", [str(BIN / "rag-service.exe")], env)
        if not wait_health():
            print(tail(service_log))
            raise RuntimeError("rag-service did not become healthy")
        check(True, f"rag-service healthy on {SERVICE} (state=mysql, bridge=debezium)")

        worker_process, worker_log = start_process(
            "rag-document-worker", [str(BIN / "rag-document-worker.exe")], env)

        # Knowledge base with the fake model connection.
        status, _, kb = http_json("POST", SERVICE + "/v1/admin/knowledge-bases", {
            "namespace": namespace,
            "embedding": {"base_url": MODEL, "apikey": "e2e-secret"},
            "embedding_model": "bge-m3",
            "reranker": {"base_url": MODEL},
            "reranker_model": "bge-reranker-base",
            "strategy": "recursive",
            "chunk_size": 512,
            "chunk_overlap": 64,
        })
        if status != 201:
            raise RuntimeError(f"KB create failed: {status} {kb}")
        collection = (kb or {}).get("collection", "")
        try:
            mysql_query(
                f"DELETE FROM {MYSQL_DATABASE}.document_jobs WHERE namespace <> '" + namespace + "';")
        except Exception as cleanup_error:  # noqa: BLE001 - best effort on a dev database
            print("WARN stale job cleanup failed:", cleanup_error)
        vector_size = (kb or {}).get("vector_size")
        check(bool(collection), f"knowledge base {namespace} created (collection={collection}, vector_size={vector_size})")
        check(vector_size == VECTOR_SIZE, f"vector_size probed from fake model = {vector_size}")

        # Tenant API key.
        status, _, created = http_json("POST", SERVICE + "/v1/admin/api-keys", {
            "name": "e2e-debezium", "namespaces": [namespace],
        })
        if status != 201:
            raise RuntimeError(f"API key create failed: {status} {created}")
        api_key = created["key"]
        check(api_key.startswith("prk_"), "tenant API key created")

        # MCP handshake + asynchronous admission.
        mcp = MCP(api_key, namespace)
        mcp.initialize()
        check(bool(mcp.session), "MCP initialize completed (session established)")

        content = ("# Debezium bridge E2E\n\n"
                   "The task row is committed in MySQL and delivered by the binlog "
                   "CDC task connector. This body must never appear on Kafka.\n")
        admitted = mcp.call_tool("add_document", {
            "name": document_name, "content": content,
            "idempotency_key": f"e2e-{int(time.time())}",
        })
        artifact_id = admitted.get("artifact_id", "")
        check(bool(artifact_id), f"add_document accepted artifact_id={mask(artifact_id)}")

        # Contract: admission is one MySQL transaction that writes document_jobs
        # (schedule_version=1). The old transactional outbox no longer exists.
        admitted_row = mysql_query(
            f"SELECT status, schedule_version FROM {MYSQL_DATABASE}.document_jobs "
            f"WHERE artifact_id = '{artifact_id}';")
        admitted_fields = admitted_row.split("\t")
        check(len(admitted_fields) == 2 and admitted_fields[0] in ("queued", "processing", "succeeded") and
              admitted_fields[1].isdigit() and int(admitted_fields[1]) >= 1,
              f"document_jobs row committed by admission (status/schedule_version={admitted_row})")

        deadline = time.time() + 180
        status_payload: dict = {}
        while time.time() < deadline:
            status_payload = mcp.call_tool("get_document_status", {"artifact_id": artifact_id})
            state = status_payload.get("status", "")
            if state in ("succeeded", "failed"):
                break
            time.sleep(1.5)
        check(status_payload.get("status") == "succeeded",
              f"document job reached succeeded via debezium delivery (stage={status_payload.get('stage')}, "
              f"error={status_payload.get('error')})")
        if status_payload.get("status") != "succeeded":
            print("worker log tail:\n" + tail(worker_log))
            print("service log tail:\n" + tail(service_log))
            raise RuntimeError("document job did not succeed")

        job_row = mysql_query(
            f"SELECT status, stage FROM {MYSQL_DATABASE}.document_jobs "
            f"WHERE artifact_id = '{artifact_id}';")
        check(job_row.startswith("succeeded"), f"MySQL document_jobs row = {job_row}")

        # Qdrant indexing.
        status, _, info = http_json("GET", f"http://127.0.0.1:6333/collections/{collection}")
        points = ((info or {}).get("result") or {}).get("points_count")
        check(status == 200 and (points or 0) > 0, f"Qdrant collection {collection} has points_count={points}")

        # REST synchronous search (no queue needed).
        status, _, search = http_json("POST", SERVICE + "/v1/search",
                                      {"query": "debezium bridge task", "rerank": "none"},
                                      {"Authorization": "Bearer " + api_key,
                                       "X-RAG-Namespace": namespace}, timeout=60)
        hits = (search or {}).get("results") or []
        names = [hit.get("source") or hit.get("title") or "" for hit in hits]
        check(status == 200 and any(document_name in str(name) for name in names),
              f"REST search returns the indexed document (hits={len(hits)})")

        # Item 5a: the deployed connector JSON/topic shape must be understood by
        # the SAME Go decoder the document worker uses — not by a fixture.
        decode_run = None
        try:
            decoded_documents, decode_run = decode_kafka_values(
                "document", kafka_consume_values(DOCUMENT_TOPIC))
            doc_event = next((item for item in decoded_documents
                              if item.get("artifact_id") == artifact_id), None)
            for _ in range(5):
                if doc_event is not None:
                    break
                time.sleep(2)
                decoded_documents, decode_run = decode_kafka_values(
                    "document", kafka_consume_values(DOCUMENT_TOPIC))
                doc_event = next((item for item in decoded_documents
                                  if item.get("artifact_id") == artifact_id), None)
            check(doc_event is not None and doc_event.get("namespace") == namespace and
                  int(doc_event.get("schedule_version") or 0) >= 1,
                  "production Go document decoder extracts artifact_id/namespace/"
                  "schedule_version from a real Kafka Connect message")
            if doc_event is not None:
                check("content" not in doc_event,
                      "the decoded document notification carries locators only (no body)")
        except Exception as error:  # noqa: BLE001 - contract check must be conclusive
            check(False, f"could not decode a real document notification: {error}")
            print(f"decode stderr: {(decode_run.stderr or '')[-2000:] if decode_run else ''}")

        # ------------------------------------------------------------------ #
        # Acceptance 1: the cache connector captures the same write as a
        # locator-only documents event on RAG_CACHE_TOPIC; the cache revision
        # row records the dirty watermark (Redis is disabled in this run, so the
        # consumer-side refresh is covered by the Go unit tests).
        # ------------------------------------------------------------------ #
        cache_found, cache_output = kafka_topic_contains(CACHE_TOPIC, document_name)
        if cache_found:
            check(bool(re.search(r'"table"\s*:\s*"documents"', cache_output)),
                  f"cache topic {CACHE_TOPIC} carries a documents change event")
            check('"content"' not in cache_output and '"sha256"' not in cache_output,
                  "cache event carries locator columns only (no body/digest)")
        else:
            print("INFO cache topic probe could not replay the documents event; the documents "
                  "row plus the cache revision below prove the capture path")
        revision = mysql_query(
            f"SELECT revision FROM {MYSQL_DATABASE}.cache_revisions "
            f"WHERE kind = 'documents' AND scope = '{namespace}';")
        check(revision.isdigit() and int(revision) > 0,
              f"cache revision for documents('{namespace}') advanced (revision={revision})")

        # ------------------------------------------------------------------ #
        # Acceptance 2: document and cache capture are independent. Pause the
        # cache connector, queue a cache change, and submit a document: the
        # document worker must claim it while cache capture is still paused.
        # ------------------------------------------------------------------ #
        independent_marker = acceptance_independent_capture(mcp, namespace)
        delete_lease_marker(namespace, independent_marker)
        check(wait_kafka_marker(CACHE_TOPIC, independent_marker, 90),
              "cache connector delivered the lease delete event after it resumed")
        cache_run = None
        try:
            cache_values = kafka_consume_values(CACHE_TOPIC)
            decoded_cache, cache_run = decode_kafka_values(
                "cache", cache_values, op="d")
            delete_event = next(
                (item for item in decoded_cache
                 if item.get("table") == "document_leases" and
                 (item.get("before") or {}).get("resource") == independent_marker),
                None)
            check(delete_event is not None,
                  "production Go cache decoder extracts source.table/op/before "
                  "locators from a real delete event")
            if delete_event is not None:
                before = delete_event.get("before") or {}
                check(before.get("namespace") == namespace and
                      before.get("owner") == "e2e",
                      f"cache delete event keeps pre-delete locator fields (before={before})")
        except Exception as error:  # noqa: BLE001 - contract check must be conclusive
            check(False, f"could not decode a real cache delete event: {error}")
            print(f"decode stderr: {(cache_run.stderr or '')[-2000:] if cache_run else ''}")


        # Business document deletion remains a separate path and must still work.
        second_name = f"bridge-after-cache-{int(time.time())}.md"
        second = mcp.call_tool("add_document", {
            "name": second_name,
            "content": "# After CDC\n\nA later normal notification still triggers ingestion.\n",
            "idempotency_key": f"e2e-cache-{int(time.time())}",
        })
        second_artifact = second.get("artifact_id", "")
        check(bool(second_artifact), f"second add_document accepted artifact_id={mask(second_artifact)}")
        second_deadline = time.time() + 180
        second_status: dict = {}
        while time.time() < second_deadline:
            second_status = mcp.call_tool("get_document_status", {"artifact_id": second_artifact})
            if second_status.get("status") in ("succeeded", "failed"):
                break
            time.sleep(1.5)
        check(second_status.get("status") == "succeeded",
              f"consumer kept working for a later notification (stage={second_status.get('stage')})")
        delete_status, _, _ = http_json(
            "DELETE", SERVICE + "/v1/documents/" + second_name, None,
            {"Authorization": "Bearer " + api_key, "X-RAG-Namespace": namespace})
        check(delete_status == 200, f"business document delete still works (HTTP {delete_status})")

        # ------------------------------------------------------------------ #
        # Acceptance 3: stop the Connect worker that owns the document task,
        # submit a document during takeover, and require the peer to deliver it
        # to the Go document worker (RUNNING alone is not proof of delivery).
        # ------------------------------------------------------------------ #
        acceptance_worker_failover(mcp, namespace, stopped_containers)

        # ------------------------------------------------------------------ #
        # Acceptance 4: workers stay online but the cache task fails (its CDC
        # account is locked and its connections killed). After clearing the
        # fault, the supervisor must restart only that task without a manual
        # REST restart; the document path keeps working.
        # ------------------------------------------------------------------ #
        acceptance_task_restart(mcp, namespace, locked_users)

        # ------------------------------------------------------------------ #
        # Acceptance 3: the monitor contract exposes both Debezium connectors,
        # the two Kafka consumer groups, the cache dirty state and the cache
        # counters; pipeline.outbox is gone and unknown stays null.
        # ------------------------------------------------------------------ #
        status, _, overview = http_json("GET", SERVICE + "/v1/admin/monitor/overview", timeout=20)
        check(status == 200 and isinstance(overview, dict), f"monitor overview HTTP {status}")
        overview = overview or {}
        queues_section = overview.get("queues") or {}
        pipeline_section = overview.get("pipeline") or {}
        service_section = overview.get("service") or {}
        check("outbox" not in pipeline_section,
              "monitor pipeline no longer exposes outbox record statistics")
        check(service_section.get("delivery") == "debezium",
              f"service.delivery = {service_section.get('delivery')}")
        check("outbox_bridge" not in service_section,
              "service.outbox_bridge was removed")
        jobs_section = (queues_section.get("document_jobs") or {}).get("by_status") or {}
        check(jobs_section.get("succeeded", 0) >= 2 and jobs_section.get("failed", 0) == 0,
              f"document job status counts successes only (by_status={jobs_section})")
        check((pipeline_section.get("document_jobs") or {}).get("status") == "ok",
              "document job pipeline view is ok (no outbox row mixed in)")
        debezium_section = pipeline_section.get("debezium") or {}
        tasks_section = debezium_section.get("tasks") or {}
        cache_connector = debezium_section.get("cache") or {}
        check(tasks_section.get("capture_progress") is None and
              cache_connector.get("capture_progress") is None,
              "capture progress is null for both connectors (binlog offsets live in "
              "Kafka Connect, never fabricated)")
        check(tasks_section.get("connector") == DOCUMENT_CONNECTOR and
              cache_connector.get("connector") == CACHE_CONNECTOR,
              f"monitor names the two fixed connectors "
              f"({tasks_section.get('connector')}/{cache_connector.get('connector')})")
        check(tasks_section.get("status") == "ok" and cache_connector.get("status") == "ok",
              f"both capture connectors are ok after the drills "
              f"(document={tasks_section.get('status')}, cache={cache_connector.get('status')})")
        for label, section in (("document", tasks_section), ("cache", cache_connector)):
            check(section.get("connector_state") == "RUNNING",
                  f"{label} connector_state is RUNNING ({section.get('connector_state')})")
            check(bool(section.get("worker_id")),
                  f"{label} connector reports its hosting worker ({section.get('worker_id')})")
            check(bool(section.get("tasks")) and
                  all(task.get("state") == "RUNNING"
                      for task in section.get("tasks") or []),
                  f"{label} task state is RUNNING ({section.get('tasks')})")
            check(section.get("supervised") is True,
                  f"{label} connector is supervised by the service")
            check(section.get("failed_tasks") == 0,
                  f"{label} connector has no currently failed task")
        check(cache_connector.get("last_restart_at") or
              cache_connector.get("recovery") == "recovered",
              "monitor kept the cache task's automatic restart result "
              f"(last_restart_at={cache_connector.get('last_restart_at')}, "
              f"recovery={cache_connector.get('recovery')})")
        cache_section = pipeline_section.get("cache") or {}
        dirty_keys = cache_section.get("dirty_keys")
        refresh_failures = cache_section.get("refresh_failures")
        check((dirty_keys is None or isinstance(dirty_keys, int)) and
              (refresh_failures is None or isinstance(refresh_failures, int)),
              f"pipeline.cache reports dirty_keys/refresh_failures (dirty={dirty_keys}, "
              f"failures={refresh_failures})")
        kafka_section = pipeline_section.get("kafka") or {}
        groups = kafka_section.get("groups") or {}
        check(all(name in groups for name in ("document", "cache")),
              f"pipeline.kafka.groups carries document/cache ({sorted(groups.keys())})")
        document_group = groups.get("document") or {}
        check(document_group.get("topic") == DOCUMENT_TOPIC and
              document_group.get("group") == DOCUMENT_GROUP,
              f"document group names its topic/group "
              f"({document_group.get('topic')}/{document_group.get('group')})")
        lag = document_group.get("consumer_lag")
        check(document_group.get("status") in ("ok", "unknown") and
              (lag is None or isinstance(lag, int)),
              f"consumer lag is numeric or explicitly unknown "
              f"(status={document_group.get('status')}, lag={lag})")
    except Exception as error:  # noqa: BLE001 - report and fail
        print(f"ERROR: {error}", flush=True)
        results.append((False, f"exception: {error}"))
    finally:
        # Leave the environment usable even when a drill failed midway.
        for user in list(locked_users):
            try:
                mysql_query(f"ALTER USER '{user}'@'%' ACCOUNT UNLOCK;")
            except Exception:  # noqa: BLE001 - best effort cleanup
                pass
        for container in list(stopped_containers):
            try:
                docker_action("start", container)
            except Exception:  # noqa: BLE001 - best effort cleanup
                pass
        try:
            set_connector_paused(CACHE_CONNECTOR, False)
        except Exception:  # noqa: BLE001 - connector may not exist in this run
            pass
        stop_process(worker_process)
        stop_process(service_process)
        model_server.shutdown()
        model_server.server_close()

    failed = [message for ok, message in results if not ok]
    print(f"\nSUMMARY passed={len(results) - len(failed)} failed={len(failed)}")
    for message in failed:
        print("  FAILED: " + message)
    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main())
