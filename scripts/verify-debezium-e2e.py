"""Live end-to-end verification of Debezium document delivery with the Go backend.

Pipeline under test:
  MCP add_document -> MySQL document_job_outbox COMMIT
  -> Debezium (binlog CDC) -> Kafka topic praxis_document_ingest
  -> rag-document-worker -> Qdrant + MySQL documents
  -> get_document_status = succeeded, REST search returns the document.

It also proves the application never published the notification itself:
the outbox row must still be unpublished (published_at IS NULL) while the
worker processed it, and the service log identifies Debezium delivery.

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
        # Compose publishes the Debezium health port on loopback; without it the
        # monitor page reports capture progress as unknown (never fabricated).
        "RAG_DEBEZIUM_HEALTH_URL": os.environ.get(
            "RAG_DEBEZIUM_HEALTH_URL", "http://127.0.0.1:8083/q/health"),
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


def log_occurrences(path: pathlib.Path, needle: str) -> int:
    try:
        return path.read_text("utf-8", "replace").count(needle)
    except Exception:  # noqa: BLE001
        return 0


def wait_log_occurrence(
    path: pathlib.Path, needle: str, timeout: float = 90.0, above: int = 0
) -> bool:
    """Wait until the log holds more than `above` occurrences of the needle.

    `above` distinguishes the delete event this run just produced from earlier
    outbox-cleanup events (for example the script's own stale-row cleanup).
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
        check(outbox_row.strip() == "1",
              "outbox row is still unpublished after admission (application did not publish)")

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

        service_text = tail(service_log, 400)
        check("Document outbox delivery=debezium" in service_text,
              "service log identifies Debezium document delivery")

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

        # Debezium does not mark the application's outbox row as published.
        still_pending = mysql_query(
            f"SELECT published_at IS NULL FROM {MYSQL_DATABASE}.document_job_outbox "
            f"WHERE artifact_id = '{artifact_id}';")
        check(still_pending.strip() == "1",
              "outbox row remained PENDING after processing (only Debezium published it)")

        # ------------------------------------------------------------------ #
        # Acceptance 1: outbox retention cleanup (MySQL DELETE) produces a CDC
        # delete event. The database row must be gone, but the event must be
        # ACKed as outbox cleanup: no retry, no dead letter, document intact.
        # ------------------------------------------------------------------ #
        malformed_before = log_occurrences(worker_log, "malformed document message ignored")
        ignored_before = log_occurrences(worker_log, "outbox cleanup event ignored")
        cleanup_rows = mysql_query(
            f"DELETE FROM {MYSQL_DATABASE}.document_job_outbox "
            f"WHERE artifact_id = '{artifact_id}'; SELECT ROW_COUNT();")
        deleted_rows = cleanup_rows.strip().splitlines()[-1] if cleanup_rows.strip() else ""
        check(deleted_rows == "1", f"outbox retention cleanup deleted the expired record (rows={deleted_rows})")
        remaining = mysql_query(
            f"SELECT COUNT(*) FROM {MYSQL_DATABASE}.document_job_outbox "
            f"WHERE artifact_id = '{artifact_id}';")
        check(remaining.strip() == "0", "outbox record is gone from MySQL after cleanup")

        acknowledged = wait_log_occurrence(
            worker_log, "outbox cleanup event ignored", 120, above=ignored_before)
        check(acknowledged, "worker classified the CDC delete event as outbox cleanup and ACKed it")
        # Give any (incorrect) retry/DLQ path a chance to run before asserting.
        time.sleep(3)
        malformed_after = log_occurrences(worker_log, "malformed document message ignored")
        check(malformed_after == malformed_before,
              "delete event did not take the malformed/retry path (no invalid retry)")
        check("Message retry limit reached" not in tail(worker_log, 400),
              "worker did not exhaust retries for the cleanup event")
        dlq_hit, dlq_output = kafka_topic_contains(DOCUMENT_DLQ_TOPIC, artifact_id)
        check(not dlq_hit, "document DLQ has no record for the cleaned-up outbox event")
        if dlq_hit:
            print("DLQ probe tail:\n" + dlq_output)

        # Consumer health: a later normal notification still ingests.
        second_name = f"bridge-after-cleanup-{int(time.time())}.md"
        second_content = "# After cleanup\n\nA normal notification still triggers ingestion.\n"
        second = mcp.call_tool("add_document", {
            "name": second_name, "content": second_content,
            "idempotency_key": f"e2e-cleanup-{int(time.time())}",
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
              f"consumer kept working after the cleanup event (stage={second_status.get('stage')})")

        # The already-indexed document is unaffected by the outbox cleanup.
        first_status = mcp.call_tool("get_document_status", {"artifact_id": artifact_id})
        check(first_status.get("status") == "succeeded",
              "original document job still succeeded after its outbox record was removed")
        status, _, listing = http_json(
            "GET", SERVICE + "/v1/documents", None,
            {"Authorization": "Bearer " + api_key, "X-RAG-Namespace": namespace})
        listed_names = [str(item.get("name", "")) for item in (listing or {}).get("documents", [])]
        check(status == 200 and document_name in listed_names,
              "document list still contains the original document after outbox cleanup")
        status, _, search_again = http_json(
            "POST", SERVICE + "/v1/search", {"query": "debezium bridge outbox", "rerank": "none"},
            {"Authorization": "Bearer " + api_key, "X-RAG-Namespace": namespace}, timeout=60)
        again_hits = (search_again or {}).get("results") or []
        again_names = [hit.get("source") or hit.get("title") or "" for hit in again_hits]
        check(status == 200 and any(document_name in str(name) for name in again_names),
              "REST search still returns the document after outbox cleanup")

        # Business document deletion is a separate path and must still work: the
        # outbox-cleanup filter only affects CDC messages, not REST deletes.
        delete_status, _, delete_payload = http_json(
            "DELETE", SERVICE + "/v1/documents/" + second_name, None,
            {"Authorization": "Bearer " + api_key, "X-RAG-Namespace": namespace})
        check(delete_status == 200, f"business document delete still works (HTTP {delete_status})")
        status, _, listing_after_delete = http_json(
            "GET", SERVICE + "/v1/documents", None,
            {"Authorization": "Bearer " + api_key, "X-RAG-Namespace": namespace})
        names_after_delete = [
            str(item.get("name", "")) for item in (listing_after_delete or {}).get("documents", [])]
        check(status == 200 and second_name not in names_after_delete and document_name in names_after_delete,
              "business delete removed only the requested document; the cleaned-up outbox document remains")

        # ------------------------------------------------------------------ #
        # Acceptance 2: monitoring separates outbox records, Debezium capture,
        # Kafka consumption and document job status; unknown stays unknown.
        # ------------------------------------------------------------------ #
        status, _, overview = http_json("GET", SERVICE + "/v1/admin/monitor/overview", timeout=20)
        check(status == 200 and isinstance(overview, dict), f"monitor overview HTTP {status}")
        overview = overview or {}
        queues_section = overview.get("queues") or {}
        pipeline_section = overview.get("pipeline") or {}
        outbox_section = pipeline_section.get("outbox") or {}
        outbox_status = outbox_section.get("by_status") or {}
        jobs_section = (queues_section.get("document_jobs") or {}).get("by_status") or {}
        check("document_outbox" not in queues_section,
              "queues no longer presents document_outbox as a backlog queue")
        check("oldest_pending_seconds" not in json.dumps(overview),
              "monitor response no longer reports oldest_pending_seconds")
        check(isinstance(outbox_section.get("total"), int) and
              outbox_section.get("total") == sum(int(value) for value in outbox_status.values()),
              f"outbox record stats are self-consistent (total={outbox_section.get('total')}, "
              f"by_status={outbox_status})")
        check(jobs_section.get("succeeded", 0) >= 2 and jobs_section.get("failed", 0) == 0,
              f"document job status counts successes only (by_status={jobs_section})")
        check((pipeline_section.get("document_jobs") or {}).get("status") == "ok",
              "succeeded jobs stay ok while retained outbox rows are PENDING (not misreported)")
        debezium_section = pipeline_section.get("debezium") or {}
        check(debezium_section.get("capture_progress") is None,
              "Debezium capture progress is reported as unknown (null), never fabricated")
        check(debezium_section.get("status") == "ok",
              f"Debezium health probe reports ok (detail={debezium_section.get('detail')})")
        kafka_section = pipeline_section.get("kafka") or {}
        check(kafka_section.get("topic") == DOCUMENT_TOPIC and kafka_section.get("group") == DOCUMENT_GROUP,
              f"Kafka progress names the document topic/group "
              f"({kafka_section.get('topic')}/{kafka_section.get('group')})")
        lag = kafka_section.get("consumer_lag")
        check(kafka_section.get("status") in ("ok", "unknown") and
              (lag is None or isinstance(lag, int)),
              f"Kafka consumer lag is numeric or explicitly unknown "
              f"(status={kafka_section.get('status')}, lag={lag})")
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





