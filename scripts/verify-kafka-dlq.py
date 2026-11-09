"""Live Kafka DLQ check: poison document message -> in-process retries -> DLQ topic."""
import pathlib
import subprocess
import sys
import time

import importlib.util
_SPEC = importlib.util.spec_from_file_location("verify_e2e", pathlib.Path(__file__).resolve().parents[1] / "scripts" / "verify-debezium-e2e.py")
e2e = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(e2e)  # noqa: E402


def docker(*args, input_bytes=None):
    return subprocess.run(["docker", *args], capture_output=True, text=True,
                          input=None if input_bytes is None else input_bytes.decode())


def main() -> int:
    e2e.WORK.mkdir(parents=True, exist_ok=True)
    (e2e.WORK / "data").mkdir(parents=True, exist_ok=True)
    (e2e.WORK / "config.toml").write_text("[environment]\n", encoding="utf-8")
    process = None
    try:
        env = e2e.base_env()
        process, log = e2e.start_process("dlq-worker", [str(e2e.BIN / "rag-document-worker.exe")], env)
        deadline = time.time() + 30
        while time.time() < deadline:
            if "document worker starting" in e2e.tail(log, 100):
                break
            time.sleep(0.5)
        print("worker started")
        marker = f"kafka-dlq-{int(time.time())}"
        poison = '{"poison":"' + marker + '"}'
        produce = docker(
            "exec", "-i", "praxis-rag-kafka", "/opt/kafka/bin/kafka-console-producer.sh",
            "--bootstrap-server", "localhost:9092", "--topic", e2e.DOCUMENT_TOPIC,
            input_bytes=poison.encode())
        if produce.returncode != 0:
            raise RuntimeError(f"producer failed: {produce.stderr[-500:]}")
        print(f"poison message produced: {marker}")

        dlq = e2e.os.environ.get("RAG_DOCUMENT_DLQ_TOPIC", "praxis_document_ingest_dead_letters")
        found = False
        output = ""
        deadline = time.time() + 60
        while time.time() < deadline and not found:
            consume = docker(
                "exec", "praxis-rag-kafka", "/opt/kafka/bin/kafka-console-consumer.sh",
                "--bootstrap-server", "localhost:9092", "--topic", dlq,
                "--from-beginning", "--timeout-ms", "5000")
            output = (consume.stdout or "") + (consume.stderr or "")
            found = marker in output
            if not found:
                time.sleep(3)
        print("DLQ marker found:", found)
        worker_text = e2e.tail(log, 200)
        retried = "document processing deferred" in worker_text or "malformed" in worker_text
        print("worker retried/exhausted poison:", retried)
        print("PASS" if found else "FAIL", "DLQ topic received the exhausted poison payload")
        print("--- worker log tail ---")
        print(e2e.tail(log, 12))
        if not found:
            print("--- DLQ consumer output tail ---")
            print(output[-2000:])
        return 0 if found else 1
    finally:
        e2e.stop_process(process)


if __name__ == "__main__":
    sys.exit(main())

