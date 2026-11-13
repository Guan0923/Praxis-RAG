"""Live Kafka queued-search check: MCP search -> Kafka -> search worker -> snapshot."""
import base64
import importlib.util
import json
import os
import pathlib
import subprocess
import sys
import time
import urllib.error
import urllib.request

_ROOT = pathlib.Path(__file__).resolve().parents[1]
_SPEC = importlib.util.spec_from_file_location("verify_e2e", _ROOT / "scripts" / "verify-debezium-e2e.py")
e2e = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(e2e)  # noqa: E402


def upload_and_wait(api_key: str, namespace: str, name: str, content: str):
    boundary = "----kafka-search-boundary"
    body = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="files"; filename="{name}"\r\n'
        "Content-Type: text/markdown\r\n\r\n" + content + "\r\n"
        f"--{boundary}--\r\n"
    ).encode()
    request = urllib.request.Request(
        e2e.SERVICE + "/v1/document-batches", data=body, method="POST",
        headers={"Authorization": "Bearer " + api_key, "X-RAG-Namespace": namespace,
                 "Content-Type": f"multipart/form-data; boundary={boundary}"})
    with urllib.request.urlopen(request, timeout=120) as response:
        status = response.status
        raw = response.read().decode()
    payload = json.loads(raw)
    if status != 202 or payload.get("accepted") != 1:
        raise RuntimeError(f"batch upload failed: {status} {payload}")
    artifact_id = payload["items"][0]["artifact_id"]
    headers = {"Authorization": "Bearer " + api_key, "X-RAG-Namespace": namespace}
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        job_status, _, job = e2e.http_json(
            "GET", e2e.SERVICE + "/v1/document-jobs/" + artifact_id, headers=headers)
        if job_status != 200 or job.get("status") == "failed":
            raise RuntimeError(f"document indexing failed: {job_status} {job}")
        if job.get("status") == "succeeded":
            return status, raw
        time.sleep(0.5)
    raise TimeoutError("document indexing did not finish within 120 seconds")


def _ensure_search_database() -> str:
    """Create a dedicated MySQL database so stale journal rows from other runs cannot interfere."""
    database = os.environ.get("RAG_E2E_SEARCH_DATABASE", "praxis_search_e2e")
    sql = "CREATE DATABASE IF NOT EXISTS {}; GRANT ALL PRIVILEGES ON {}.* TO 'rag'@'%'; FLUSH PRIVILEGES;".format(
        database, database)
    completed = subprocess.run(
        ["docker", "exec", "praxis-rag-mysql", "sh", "-c",
         'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -e "{}"'.format(sql)],
        capture_output=True, text=True, timeout=60)
    if completed.returncode != 0:
        raise RuntimeError("cannot prepare {}: {}".format(database, completed.stderr[-500:]))
    return database


def main() -> int:
    e2e.WORK.mkdir(parents=True, exist_ok=True)
    (e2e.WORK / "data").mkdir(parents=True, exist_ok=True)
    (e2e.WORK / "config.toml").write_text("[environment]\n", encoding="utf-8")
    model = e2e.start_model_server()
    service = worker = document_worker = None
    try:
        search_database = _ensure_search_database()
        env = e2e.base_env()
        env["RAG_MYSQL_DATABASE"] = search_database
        env["RAG_REDIS_ENABLED"] = "true"
        env["RAG_REDIS_URL"] = "redis://127.0.0.1:6379/0"
        env["RAG_REDIS_PREFIX"] = "rag:e2e:search:" + time.strftime("%H%M%S")
        service, service_log = e2e.start_process("search-service", [str(e2e.BIN / "rag-service.exe")], env)
        if not e2e.wait_health():
            print(e2e.tail(service_log))
            return 1
        print("service healthy")
        worker, worker_log = e2e.start_process("search-worker", [str(e2e.BIN / "rag-search-worker.exe")], env)
        document_worker, _ = e2e.start_process("document-worker", [str(e2e.BIN / "rag-document-worker.exe")], env)

        namespace = "gosearch" + time.strftime("%H%M%S")
        status, _, kb = e2e.http_json("POST", e2e.SERVICE + "/v1/admin/knowledge-bases", {
            "namespace": namespace,
            "embedding": {"base_url": e2e.MODEL, "apikey": "search"},
            "embedding_model": "bge-m3",
            "reranker": {"base_url": e2e.MODEL},
            "reranker_model": "bge-reranker-base",
            "strategy": "recursive", "chunk_size": 512, "chunk_overlap": 64,
        })
        if status != 201:
            raise RuntimeError(f"KB create failed: {status} {kb}")
        status, _, created = e2e.http_json("POST", e2e.SERVICE + "/v1/admin/api-keys",
                                          {"name": "gosearch", "namespaces": [namespace]})
        api_key = created["key"]
        document = f"kafka-search-{int(time.time())}.md"
        upload_status, upload_body = upload_and_wait(
            api_key, namespace, document,
            "# Kafka queued search\n\nThe search worker consumes the Kafka envelope.\n")
        print("upload", upload_status, upload_body[:160])

        mcp = e2e.MCP(api_key, namespace)
        mcp.initialize()
        payload = mcp.call_tool("search", {"query": "kafka queued search worker", "wait_seconds": 30})
        print("search tool payload keys:", sorted(payload.keys()))
        result = payload
        if "result_id" in payload and "results" not in payload:
            deadline = time.time() + 60
            while time.time() < deadline:
                result = mcp.call_tool("get_search_result", {"result_id": payload["result_id"]})
                if result.get("results") is not None:
                    break
                time.sleep(1)
        hits = result.get("results") or []
        names = [str(hit.get("source") or hit.get("title") or "") for hit in hits]
        ok = any(document in name for name in names)
        print("search hits:", len(hits), "match:", ok, "snapshot_available:", result.get("snapshot_available"))
        worker_text = e2e.tail(worker_log, 60)
        print("worker consumed envelope:", "search" in worker_text or "claimed" in worker_text)
        print("PASS" if ok else "FAIL", "queued MCP search completed through Kafka")
        if not ok:
            print("--- service log tail ---\n" + e2e.tail(service_log, 30))
            print("--- worker log tail ---\n" + worker_text)
        return 0 if ok else 1
    except Exception as error:  # noqa: BLE001 - report and fail
        print("ERROR:", error)
        return 1
    finally:
        e2e.stop_process(worker)
        e2e.stop_process(document_worker)
        e2e.stop_process(service)
        model.shutdown()
        model.server_close()


if __name__ == "__main__":
    sys.exit(main())
