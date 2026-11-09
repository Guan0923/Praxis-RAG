"""Live end-to-end verification of RAG_OUTBOX_BRIDGE=debezium with the Go backend.

Pipeline under test:
  MCP add_document -> MySQL document_job_outbox COMMIT
  -> Debezium (binlog CDC) -> Kafka topic praxis_document_ingest
  -> rag-document-worker -> Qdrant + MySQL documents
  -> get_document_status = succeeded, REST search returns the document.

It also proves the application never published the notification itself:
the outbox row must still be unpublished (published_at IS NULL) while the
worker processed it, and the service log must contain the debezium-mode line.

Requires the docker compose infra (MySQL 3307, Qdrant 6333, Redis 6379,
Kafka 9092, Debezium) to be running, and dist/bin binaries built.
"""
from __future__ import annotations

import base64
import json
import os
import pathlib
import random
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
BRIDGE = os.environ.get("RAG_E2E_BRIDGE", "debezium")
if BRIDGE not in ("relay", "debezium"):
    raise SystemExit("RAG_E2E_BRIDGE must be relay or debezium")
KAFKA_BOOTSTRAP = os.environ.get("RAG_KAFKA_BOOTSTRAP_SERVERS", "127.0.0.1:9092")
CACHE_TOPIC = os.environ.get("RAG_CACHE_TOPIC", "praxis_state_cache_refresh")
CACHE_GROUP = os.environ.get("RAG_CACHE_GROUP", "praxis_state_cache_refresh_workers")
DOCUMENT_TOPIC = os.environ.get("RAG_DOCUMENT_TOPIC", "praxis_document_ingest")
DOCUMENT_GROUP = os.environ.get("RAG_DOCUMENT_GROUP", "praxis_document_ingest_workers")
SEARCH_TOPIC = os.environ.get("RAG_SEARCH_TOPIC", "praxis_search")
SEARCH_GROUP = os.environ.get("RAG_SEARCH_GROUP", "praxis_search_workers")
DOCUMENT_DLQ_TOPIC = os.environ.get(
    "RAG_DOCUMENT_DLQ_TOPIC", "praxis_document_ingest_dead_letters")
SEARCH_DLQ_TOPIC = os.environ.get("RAG_SEARCH_DLQ_TOPIC", "praxis_search_dead_letters")
DOCUMENT_DLQ_GROUP = os.environ.get(
    "RAG_DOCUMENT_DLQ_GROUP", "praxis_document_dead_letters")
SEARCH_DLQ_GROUP = os.environ.get("RAG_SEARCH_DLQ_GROUP", "praxis_search_dead_letters")

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
        "RAG_OUTBOX_BRIDGE": BRIDGE,
        "RAG_CACHE_TOPIC": CACHE_TOPIC,
        "RAG_CACHE_GROUP": CACHE_GROUP,
        "RAG_DOCUMENT_TOPIC": DOCUMENT_TOPIC,
        "RAG_DOCUMENT_GROUP": DOCUMENT_GROUP,
        "RAG_SEARCH_TOPIC": SEARCH_TOPIC,
        "RAG_SEARCH_GROUP": SEARCH_GROUP,
        "RAG_DOCUMENT_DLQ_TOPIC": DOCUMENT_DLQ_TOPIC,
        "RAG_SEARCH_DLQ_TOPIC": SEARCH_DLQ_TOPIC,
        "RAG_DOCUMENT_DLQ_GROUP": DOCUMENT_DLQ_GROUP,
        "RAG_SEARCH_DLQ_GROUP": SEARCH_DLQ_GROUP,
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


def kafka_probe(artifact_id: str) -> tuple[bool, str]:
    """Info-only probe: read the document topic from the beginning for 5 seconds.

    Kafka offsets/consumer groups decide what a fresh probe can see; a missing
    hit is not a failure because the PENDING outbox row plus succeeded worker
    already prove Debezium delivered it.
    """
    try:
        completed = subprocess.run([
            "docker", "exec", "praxis-rag-kafka",
            "/opt/kafka/bin/kafka-console-consumer.sh",
            "--bootstrap-server", "kafka:9092",
            "--topic", DOCUMENT_TOPIC,
            "--from-beginning",
            "--timeout-ms", "5000",
        ], capture_output=True, text=True, timeout=30)
        output = (completed.stdout or "") + (completed.stderr or "")
        return artifact_id in output, output[-6000:]
    except Exception as error:  # noqa: BLE001 - probe is optional evidence
        return False, f"probe failed: {error}"


def tail(path: pathlib.Path, lines: int = 40) -> str:
    try:
        content = path.read_text("utf-8", "replace").splitlines()
        return "\n".join(content[-lines:])
    except Exception:  # noqa: BLE001
        return "<no log>"


# --------------------------------------------------------------------------- #
# Main flow.
# --------------------------------------------------------------------------- #
def main() -> int:
    if not BIN.joinpath("rag-service.exe").exists():
        print("missing dist/bin binaries; run scripts/build-go.ps1 first")
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
    try:
        env = base_env()
        service_process, service_log = start_process(
            "rag-service", [str(BIN / "rag-service.exe")], env)
        if not wait_health():
            print(tail(service_log))
            raise RuntimeError("rag-service did not become healthy")
        check(True, f"rag-service healthy on {SERVICE} (state=mysql, bridge={BRIDGE})")

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
                f"DELETE FROM {MYSQL_DATABASE}.document_job_outbox WHERE namespace <> '" + namespace + "';"
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
                   "The outbox row is committed in MySQL and delivered by the binlog CDC bridge.\n")
        admitted = mcp.call_tool("add_document", {
            "name": document_name, "content": content,
            "idempotency_key": f"e2e-{int(time.time())}",
        })
        artifact_id = admitted.get("artifact_id", "")
        check(bool(artifact_id), f"add_document accepted artifact_id={mask(artifact_id)}")

        outbox_row = mysql_query(
            f"SELECT published_at IS NULL FROM {MYSQL_DATABASE}.document_job_outbox "
            f"WHERE artifact_id = '{artifact_id}';")
        if BRIDGE == "debezium":
            check(outbox_row.strip() == "1",
                  "outbox row is still unpublished after admission (application did not publish)")
        else:
            print("INFO relay mode: the in-process relay publishes the notification instead of Debezium")

        deadline = time.time() + 180
        status_payload: dict = {}
        while time.time() < deadline:
            status_payload = mcp.call_tool("get_document_status", {"artifact_id": artifact_id})
            state = status_payload.get("status", "")
            if state in ("succeeded", "failed"):
                break
            time.sleep(1.5)
        check(status_payload.get("status") == "succeeded",
              f"document job reached succeeded via {BRIDGE} delivery (stage={status_payload.get('stage')}, "
              f"error={status_payload.get('error')})")
        if status_payload.get("status") != "succeeded":
            print("worker log tail:\n" + tail(worker_log))
            print("service log tail:\n" + tail(service_log))
            raise RuntimeError("document job did not succeed")

        job_row = mysql_query(
            f"SELECT status, stage FROM {MYSQL_DATABASE}.document_jobs "
            f"WHERE artifact_id = '{artifact_id}';")
        check(job_row.startswith("succeeded"), f"MySQL document_jobs row = {job_row}")

        service_text = tail(service_log, 400)
        if BRIDGE == "debezium":
            check("bridge mode=debezium" in service_text and "relay_enabled=false" in service_text,
                  "service log confirms the in-process relay is disabled in debezium mode")
        else:
            check("bridge mode=relay" in service_text and "relay_enabled=true" in service_text,
                  "service log confirms the in-process relay is enabled in relay mode")

        # Qdrant indexing.
        status, _, info = http_json("GET", f"http://127.0.0.1:6333/collections/{collection}")
        points = ((info or {}).get("result") or {}).get("points_count")
        check(status == 200 and (points or 0) > 0, f"Qdrant collection {collection} has points_count={points}")

        # REST synchronous search (no queue needed).
        status, _, search = http_json("POST", SERVICE + "/v1/search",
                                      {"query": "debezium bridge outbox", "rerank": "none"},
                                      {"Authorization": "Bearer " + api_key,
                                       "X-RAG-Namespace": namespace}, timeout=60)
        hits = (search or {}).get("results") or []
        names = [hit.get("source") or hit.get("title") or "" for hit in hits]
        check(status == 200 and any(document_name in str(name) for name in names),
              f"REST search returns the indexed document (hits={len(hits)})")

        delivered, probe_output = kafka_probe(artifact_id)
        if delivered:
            print(f"PASS topic probe found the artifact notification in {DOCUMENT_TOPIC}")
        else:
            print("INFO kafka-console-consumer could not replay the artifact message "
                  "for this probe; the PENDING outbox row plus the succeeded worker "
                  "are the conclusive evidence that only Debezium delivered it")

        # Evidence about the outbox row: only Debezium publishes in debezium mode;
        # the in-process relay marks it PUBLISHED in relay mode.
        if BRIDGE == "debezium":
            still_pending = mysql_query(
                f"SELECT published_at IS NULL FROM {MYSQL_DATABASE}.document_job_outbox "
                f"WHERE artifact_id = '{artifact_id}';")
            check(still_pending.strip() == "1",
                  "outbox row remained PENDING after processing (only Debezium published it)")
        else:
            published = mysql_query(
                f"SELECT published_at IS NOT NULL FROM {MYSQL_DATABASE}.document_job_outbox "
                f"WHERE artifact_id = '{artifact_id}';")
            check(published.strip() == "1",
                  "outbox row was marked PUBLISHED by the in-process relay")
    except Exception as error:  # noqa: BLE001 - report and fail
        print(f"ERROR: {error}", flush=True)
        results.append((False, f"exception: {error}"))
    finally:
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





