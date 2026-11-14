"""Live end-to-end verification of the Debezium CDC pipeline with the Go backend.

Pipeline under test (no transactional outbox any more):
  MCP add_document -> MySQL document_jobs COMMIT
  -> task Debezium connector (binlog CDC) -> Kafka RAG_DOCUMENT_TOPIC
  -> rag-document-worker -> Qdrant + MySQL documents
  -> get_document_status = succeeded, REST search returns the document.
  The same write is captured by the cache Debezium connector as a locator-only
  event on RAG_CACHE_TOPIC (documents table, no body), and the cache revision
  row advances.
  Search is synchronous now (MCP/REST return evidence in one call): there is no
  search worker, result_id or result snapshot to poll in this pipeline check.

Also asserts the Console monitor contract: pipeline.debezium.tasks/cache,
pipeline.kafka.groups (document/cache), pipeline.cache and
service.delivery=debezium; pipeline.outbox must be gone.

Requires the docker compose infra (MySQL 3307, Qdrant 6333, Redis 6379,
Kafka 9092, both Debezium connectors) to be running, and dist/bin binaries built.
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
# Loopback health endpoints published by compose for the two connectors.
DEBEZIUM_HEALTH_URL = os.environ.get(
    "RAG_DEBEZIUM_HEALTH_URL", "http://127.0.0.1:8083/q/health")
DEBEZIUM_CACHE_HEALTH_URL = os.environ.get(
    "RAG_DEBEZIUM_CACHE_HEALTH_URL", "http://127.0.0.1:8084/q/health")

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
        # Compose publishes one health port per connector on loopback; without
        # them the monitor page reports capture progress as unknown (never
        # fabricated).
        "RAG_DEBEZIUM_HEALTH_URL": DEBEZIUM_HEALTH_URL,
        "RAG_DEBEZIUM_CACHE_HEALTH_URL": DEBEZIUM_CACHE_HEALTH_URL,
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
    hit is not a failure because the succeeded document job plus the committed
    document_jobs row already prove the task connector delivered it.
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

        # Schema-4 task notification: locator columns only, no body.
        delivered, probe_output = kafka_probe(artifact_id)
        if delivered:
            check('"schedule_version"' in probe_output,
                  f"task topic event carries the schema-4 schedule_version locator in {DOCUMENT_TOPIC}")
            check('"content"' not in probe_output,
                  "task topic event carries no document body (column.include.list pruned)")
        else:
            print("INFO kafka-console-consumer could not replay the artifact message for this probe; "
                  "the succeeded job plus the committed document_jobs row are the conclusive evidence "
                  "that the task connector delivered it")

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
              "Debezium capture progress is null for both connectors (never fabricated)")
        check(tasks_section.get("status") in ("ok", "unknown"),
              f"task connector health is ok/unknown (status={tasks_section.get('status')})")
        check(cache_connector.get("status") in ("ok", "unknown"),
              f"cache connector health is ok/unknown (status={cache_connector.get('status')})")
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





