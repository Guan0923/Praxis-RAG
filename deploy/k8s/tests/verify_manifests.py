#!/usr/bin/env python3
"""Offline contract checks for the Kubernetes manifests under deploy/k8s.

Renders every kustomization with `kubectl kustomize` and asserts the deployment
contract this directory promises: container commands, images, Service selectors,
Secret key references, DNS names, probes, resources and security context.

It never contacts a cluster, so it does not replace a server-side dry-run or a
real install; see deploy/k8s/README.md §6 (install), §15 (scope).

Requirements: python3 + PyYAML, and kubectl (with built-in kustomize) on PATH.

    python3 deploy/k8s/tests/verify_manifests.py

Exit code 0 means every assertion passed.
"""

from __future__ import annotations

import pathlib
import subprocess
import sys

try:
    import yaml
except ImportError:  # pragma: no cover - environment problem, not a manifest one
    print("verify_manifests: PyYAML is required (pip install pyyaml)", file=sys.stderr)
    raise SystemExit(2)

K8S_ROOT = pathlib.Path(__file__).resolve().parent.parent

SECRET_NAME = "praxis-rag-secrets"
CONFIGMAP_NAME = "praxis-rag-config"
NAMESPACE = "praxis-rag"
GO_IMAGE = "praxis-rag"
PDF_IMAGE = "praxis-rag-pdf-parser"
TAG = "1.0.0"

# Secret keys the manifests may reference (see README §4.1).
ALLOWED_SECRET_KEYS = {
    "RAG_SERVICE_TOKEN",
    "RAG_REDIS_LOCK_URL", "RAG_LIGHTRAG_SERVICE_TOKEN", "RAG_KAFKA_SASL_PASSWORD",
    "RAG_MYSQL_PASSWORD",
    "RAG_STATE_ENCRYPTION_KEY",
    "RAG_MYSQL_DEBEZIUM_PASSWORD",
    "RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD",
    "RAG_REDIS_URL",
    "RAG_VLM_API_KEY",
    "RAG_MIGRATE_MYSQL_USER",
    "RAG_MIGRATE_MYSQL_PASSWORD",
}

DOC_WORKER = "rag-document-worker"
SAG_WORKER = "rag-sag-worker"
API = "rag-service"
PDF = "pdf-parser"
CACHE_WORKER = "rag-cache-worker"

failures: list[str] = []
checks = 0


def check(condition: bool, message: str) -> None:
    global checks
    checks += 1
    if not condition:
        failures.append(message)


def render(entry: str) -> list[dict]:
    """Render one kustomization directory with kubectl."""
    path = K8S_ROOT / entry
    result = subprocess.run(
        ["kubectl", "kustomize", str(path)],
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    check(result.returncode == 0, f"{entry}: kubectl kustomize failed: {result.stderr.strip()[:400]}")
    if result.returncode != 0:
        return []
    documents = [doc for doc in yaml.safe_load_all(result.stdout) if doc]
    check(bool(documents), f"{entry}: rendered no objects")
    return documents


def of_kind(documents: list[dict], kind: str) -> list[dict]:
    return [doc for doc in documents if doc.get("kind") == kind]


def find(documents: list[dict], name: str) -> dict | None:
    for doc in documents:
        if doc.get("metadata", {}).get("name") == name:
            return doc
    return None


def pod_of(deployment: dict) -> dict:
    containers = deployment["spec"]["template"]["spec"]["containers"]
    check(len(containers) == 1, f"{deployment['metadata']['name']}: expected exactly one container")
    return containers[0]


def env_of(container: dict) -> dict[str, dict]:
    return {item["name"]: item for item in container.get("env", [])}


def secret_keys(container: dict, where: str) -> set[str]:
    keys: set[str] = set()
    for item in container.get("env", []):
        ref = item.get("valueFrom", {}).get("secretKeyRef")
        if not ref:
            continue
        check(ref["name"] == SECRET_NAME, f"{where}: secret name {ref['name']} != {SECRET_NAME}")
        check(ref["key"] in ALLOWED_SECRET_KEYS, f"{where}: unexpected secret key {ref['key']}")
        keys.add(ref["key"])
    return keys


def check_go_workload(entry: str, deployment: dict) -> None:
    """Checks shared by every workload built from the Go image."""
    name = deployment["metadata"]["name"]
    where = f"{entry}/{name}"
    container = pod_of(deployment)

    image = container["image"]
    check(
        image.startswith(f"{GO_IMAGE}:") and image.endswith(f":{TAG}"),
        f"{where}: image {image} is not {GO_IMAGE}:{TAG}",
    )

    refs = [ref["configMapRef"]["name"] for ref in container.get("envFrom", []) if "configMapRef" in ref]
    check(CONFIGMAP_NAME in refs, f"{where}: envFrom does not reference {CONFIGMAP_NAME}")

    for probe, path in [("readinessProbe", "/ready"), ("livenessProbe", "/live")]:
        check(container.get(probe, {}).get("httpGet", {}).get("path") == path, f"{where}: {probe} must use {path}")
    env = env_of(container)
    # Parse staging (os.TempDir) and the upload workspace root must be a writable
    # per-pod emptyDir; see README §10.
    check(env.get("TMPDIR", {}).get("value") == "/var/tmp/rag", f"{where}: TMPDIR must be /var/tmp/rag")
    mounts = {mount["mountPath"] for mount in container.get("volumeMounts", [])}
    check("/var/tmp/rag" in mounts, f"{where}: /var/tmp/rag is not mounted")
    volumes = {volume["name"]: volume for volume in deployment["spec"]["template"]["spec"]["volumes"]}
    check("emptyDir" in volumes.get("tmp", {}), f"{where}: tmp volume must be an emptyDir")

    # The API binds a non-loopback address; workers stay loopback so the service
    # token is not required outside the API pod (README §2).
    expected_host = "0.0.0.0" if name == API else "127.0.0.1"
    check(
        env.get("RAG_SERVICE_HOST", {}).get("value") == expected_host,
        f"{where}: RAG_SERVICE_HOST must be {expected_host}",
    )

    resources = container.get("resources", {})
    check("requests" in resources and "limits" in resources, f"{where}: resources.requests/limits missing")

    security = container.get("securityContext", {})
    check(security.get("allowPrivilegeEscalation") is False, f"{where}: allowPrivilegeEscalation must be false")
    check(security.get("capabilities", {}).get("drop") == ["ALL"], f"{where}: capabilities must drop ALL")
    pod_security = deployment["spec"]["template"]["spec"].get("securityContext", {})
    check(
        "runAsNonRoot" not in pod_security and "runAsUser" not in pod_security,
        f"{where}: do not pin runAsNonRoot/runAsUser for the named `rag` user (README §2)",
    )


def check_overlay(entry: str, deployments_expected: set[str], engine: str, cache: bool) -> None:
    documents = render(entry)

    for doc in documents:
        kind = doc.get("kind")
        metadata = doc.get("metadata", {})
        expected_namespace = None if kind in {"ClusterRole", "ClusterRoleBinding"} else NAMESPACE
        check(
            metadata.get("namespace") == expected_namespace,
            f"{entry}: {kind}/{metadata.get('name')} namespace must be {expected_namespace!r}",
        )

    deployments = of_kind(documents, "Deployment")
    names = {doc["metadata"]["name"] for doc in deployments}
    check(names == deployments_expected, f"{entry}: deployments {sorted(names)} != {sorted(deployments_expected)}")

    for deployment in deployments:
        if deployment["metadata"]["name"] != PDF:
            check_go_workload(entry, deployment)

    api = find(deployments, API)
    check(api is not None, f"{entry}: {API} deployment missing")
    if api is not None:
        # Single instance first: Recreate guarantees the old pod is gone before
        # the new one starts, at the cost of a short interruption (README §13).
        check(
            api["spec"].get("strategy", {}).get("type") == "Recreate",
            f"{entry}: {API} must use strategy Recreate",
        )
        check(api["spec"]["replicas"] == 1, f"{entry}: {API} must start with one replica")
        container = pod_of(api)
        keys = secret_keys(container, f"{entry}/{API}")
        for key in ("RAG_SERVICE_TOKEN", "RAG_MYSQL_PASSWORD", "RAG_STATE_ENCRYPTION_KEY"):
            check(key in keys, f"{entry}/{API}: missing secret key {key}")
        if cache:
            check("RAG_REDIS_URL" in keys, f"{entry}/{API}: cache enabled but RAG_REDIS_URL is not injected")

    worker = find(deployments, DOC_WORKER)
    check(worker is not None, f"{entry}: {DOC_WORKER} deployment missing")
    if worker is not None:
        check(worker["spec"]["replicas"] == 1, f"{entry}: {DOC_WORKER} must start with 1 replica")
        container = pod_of(worker)
        check(
            container.get("command") == ["/app/bin/rag-document-worker"],
            f"{entry}: {DOC_WORKER} must override command to /app/bin/rag-document-worker",
        )
        check(
            "RAG_SERVICE_TOKEN" not in secret_keys(container, f"{entry}/{DOC_WORKER}"),
            f"{entry}: {DOC_WORKER} must not receive RAG_SERVICE_TOKEN",
        )

    sag_worker = find(deployments, SAG_WORKER)
    if sag_worker is not None:
        container = pod_of(sag_worker)
        check(container.get("command") == ["/app/bin/rag-sag-worker"], f"{entry}: SAG worker command")
        check(
            secret_keys(container, f"{entry}/{SAG_WORKER}") == {"RAG_MYSQL_PASSWORD", "RAG_STATE_ENCRYPTION_KEY"},
            f"{entry}: SAG worker must receive the MySQL password and state encryption key",
        )

    document_dlq = find(deployments, "rag-dead-letter-document")
    check(document_dlq is not None, f"{entry}: document dead-letter deployment missing")
    if document_dlq is not None:
        check(
            pod_of(document_dlq).get("command") == ["/app/bin/rag-dead-letter-worker", "--kind", "document"],
            f"{entry}: document dead-letter command",
        )

    if cache:
        cache_worker = find(deployments, CACHE_WORKER)
        check(cache_worker is not None, f"{entry}: cache worker deployment missing")
        if cache_worker is not None:
            check(
                pod_of(cache_worker).get("command") == ["/app/bin/rag-cache-worker"],
                f"{entry}: cache worker command",
            )
            check(
                "RAG_REDIS_URL" in secret_keys(pod_of(cache_worker), f"{entry}/{CACHE_WORKER}"),
                f"{entry}/{CACHE_WORKER}: RAG_REDIS_URL is not injected",
            )
        cache_dlq = find(deployments, "rag-dead-letter-cache")
        check(cache_dlq is not None, f"{entry}: cache dead-letter deployment missing")
        if cache_dlq is not None:
            check(
                pod_of(cache_dlq).get("command") == ["/app/bin/rag-dead-letter-worker", "--kind", "cache"],
                f"{entry}: cache dead-letter command",
            )
    else:
        check(find(deployments, CACHE_WORKER) is None, f"{entry}: cache worker leaked without the cache component")

    configmap = find(of_kind(documents, "ConfigMap"), CONFIGMAP_NAME)
    check(configmap is not None, f"{entry}: ConfigMap {CONFIGMAP_NAME} missing")
    if configmap is not None:
        data = configmap.get("data", {})
        check(data.get("RAG_PDF_ENGINE") == engine, f"{entry}: RAG_PDF_ENGINE must be {engine}")
        if engine == "vision":
            check(
                data.get("RAG_PDF_SERVICE_URL") == "http://pdf-parser:8091",
                f"{entry}: RAG_PDF_SERVICE_URL must be http://pdf-parser:8091",
            )
        else:
            check("RAG_PDF_SERVICE_URL" not in data, f"{entry}: go entry must not configure a PDF service URL")
        if cache:
            check(data.get("RAG_REDIS_ENABLED") == "true", f"{entry}: RAG_REDIS_ENABLED must be true")
        else:
            check("RAG_REDIS_ENABLED" not in data, f"{entry}: cache config leaked without the cache component")
        # Values the bootstrap Jobs and the app must agree on.
        for key in ("RAG_CONNECT_DOCUMENT_CONNECTOR", "RAG_CONNECT_CACHE_CONNECTOR", "RAG_MYSQL_DATABASE"):
            check(key in data, f"{entry}: ConfigMap is missing {key}")

    services = of_kind(documents, "Service")
    expected_services = {"rag-service"} | ({"pdf-parser"} if engine == "vision" else set())
    check(
        {service["metadata"]["name"] for service in services} == expected_services,
        f"{entry}: services {sorted(service['metadata']['name'] for service in services)} != {sorted(expected_services)}",
    )
    for service in services:
        selector = service["spec"]["selector"]
        matched = [
            deployment
            for deployment in deployments
            if all(
                deployment["spec"]["template"]["metadata"]["labels"].get(key) == value
                for key, value in selector.items()
            )
        ]
        where = f"{entry}: Service/{service['metadata']['name']}"
        check(len(matched) == 1, f"{where} selector matches {len(matched)} deployments, want 1")
        check(service["spec"]["type"] == "ClusterIP", f"{where} must be ClusterIP")
        port = service["spec"]["ports"][0]
        expected_port = 8011 if service["metadata"]["name"] == "rag-service" else 8091
        check(port["port"] == expected_port, f"{where} port {port['port']} != {expected_port}")
        if matched:
            declared = {item["name"] for item in pod_of(matched[0]).get("ports", [])}
            check(port["targetPort"] in declared, f"{where} targetPort {port['targetPort']} is not declared")

    if engine == "vision":
        parser = find(deployments, PDF)
        where = f"{entry}/{PDF}"
        check(parser is not None, f"{where} deployment missing")
        if parser is not None:
            container = pod_of(parser)
            check(
                container["image"].startswith(f"{PDF_IMAGE}:") and container["image"].endswith(f":{TAG}"),
                f"{where}: image {container['image']} is not {PDF_IMAGE}:{TAG}",
            )
            check("command" not in container, f"{where}: keep the image CMD (python -m app.main)")
            pod_security = parser["spec"]["template"]["spec"]["securityContext"]
            check(pod_security.get("runAsNonRoot") is True, f"{where}: runAsNonRoot must be true")
            check(pod_security.get("runAsUser") == 10001, f"{where}: runAsUser must be 10001")
            env = {item["name"]: item.get("value") for item in container.get("env", [])}
            check(env.get("RAG_PDF_SERVICE_WORKERS") == "1", f"{where}: WORKERS must start at 1")
            check(env.get("RAG_PDF_SERVICE_CONCURRENCY") == "1", f"{where}: CONCURRENCY must start at 1")
            check(env.get("RAG_PDF_SERVICE_CONVERTER") == "vision", f"{where}: converter must be vision")
            check(env.get("RAG_PDF_SERVICE_TIMEOUT") == "900", f"{where}: total timeout must be 900")
            check(env.get("RAG_PDF_SERVICE_PARSE_TIMEOUT") == "120", f"{where}: parse timeout must be 120")
            mounts = {mount["mountPath"] for mount in container.get("volumeMounts", [])}
            check("/tmp/rag-pdf-service" in mounts, f"{where}: TMPDIR is not mounted")
            probes = {
                probe["httpGet"]["path"]
                for probe in (container.get("readinessProbe"), container.get("livenessProbe"))
                if probe and "httpGet" in probe
            }
            check(probes == {"/healthz"}, f"{where}: probes must use /healthz, got {probes}")


def check_bootstrap(entry: str, job_name: str) -> str:
    """Checks one bootstrap entry; returns the container script for caller-specific asserts."""
    documents = render(entry)
    kinds = {doc["kind"] for doc in documents}
    # A bootstrap entry must never create application workloads: it runs before
    # them and must not start them early (README §6).
    check(kinds == {"Job"}, f"{entry}: must render exactly one Job, got {sorted(kinds)}")
    jobs = of_kind(documents, "Job")
    check(len(jobs) == 1, f"{entry}: expected one Job")
    if not jobs:
        return ""
    job = jobs[0]
    check(job["metadata"]["name"] == job_name, f"{entry}: job name {job['metadata']['name']} != {job_name}")
    check(job["metadata"].get("namespace") == NAMESPACE, f"{entry}: job namespace")
    check("backoffLimit" in job["spec"], f"{entry}: backoffLimit missing")
    pod = job["spec"]["template"]["spec"]
    check(pod.get("restartPolicy") == "Never", f"{entry}: restartPolicy must be Never")
    container = pod["containers"][0]
    secret_keys(container, entry)
    return " ".join(container.get("args") or []) + " " + " ".join(container.get("command") or [])


def check_common() -> None:
    documents = render("common")
    kinds = [doc["kind"] for doc in documents]
    check(kinds == ["ConfigMap"], f"common: expected only a ConfigMap, got {kinds}")
    configmap = find(documents, CONFIGMAP_NAME)
    check(configmap is not None, f"common: ConfigMap {CONFIGMAP_NAME} missing")
    if configmap is None:
        return
    data = configmap["data"]
    # Placeholders that must be replaced before a real install.
    check(
        data.get("RAG_BACKEND") == "qdrant",
        "common: RAG_BACKEND must be qdrant",
    )
    check(data.get("RAG_STATE_AUTO_SCHEMA") == "false", "common: application pods must not run DDL")
    for key in ("QDRANT_URL", "RAG_MYSQL_HOST", "RAG_KAFKA_BOOTSTRAP_SERVERS", "RAG_CONNECT_URLS"):
        check(key in data, f"common: {key} missing")
        check(bool(data.get(key)), f"common: {key} empty")
    # The MCP allowlists must stay explicit (the code requires them off-loopback).
    check(bool(data.get("RAG_MCP_ALLOWED_HOSTS")), "common: RAG_MCP_ALLOWED_HOSTS missing")
    check(bool(data.get("RAG_MCP_ALLOWED_ORIGINS")), "common: RAG_MCP_ALLOWED_ORIGINS missing")


def main() -> int:
    check_common()

    check_overlay(
        "overlays/go",
        {API, DOC_WORKER, SAG_WORKER, "rag-reembed-worker", "rag-delete-worker", "rag-dead-letter-document"},
        engine="go",
        cache=False,
    )
    check_overlay(
        "overlays/vision",
        {API, DOC_WORKER, SAG_WORKER, "rag-reembed-worker", "rag-delete-worker", "rag-dead-letter-document", PDF},
        engine="vision",
        cache=False,
    )
    check_overlay(
        "overlays/go-cache",
        {API, DOC_WORKER, SAG_WORKER, "rag-reembed-worker", "rag-delete-worker", "rag-dead-letter-document", CACHE_WORKER, "rag-dead-letter-cache"},
        engine="go",
        cache=True,
    )
    check_overlay(
        "overlays/vision-cache",
        {API, DOC_WORKER, SAG_WORKER, "rag-reembed-worker", "rag-delete-worker", "rag-dead-letter-document", PDF, CACHE_WORKER, "rag-dead-letter-cache"},
        engine="vision",
        cache=True,
    )

    script = check_bootstrap("bootstrap/state-check", "rag-state-check")
    check("--apply" not in script, "bootstrap/state-check: must stay report-only")

    script = check_bootstrap("bootstrap/state-migrate", "rag-state-migrate")
    check("/app/bin/rag-state-migrate --apply" in script, "bootstrap/state-migrate: --apply missing")
    check("RAG_MIGRATE_MYSQL_PASSWORD" in script, "bootstrap/state-migrate: migrate account override missing")

    script = check_bootstrap("bootstrap/connect-precheck", "rag-connect-precheck")
    check("rag-connect plugins" in script, "bootstrap/connect-precheck: plugin version check missing")
    check("rag-connect status" in script, "bootstrap/connect-precheck: connector status check missing")

    script = check_bootstrap("bootstrap/connect-register", "rag-connect-register")
    # --wait is a Go duration flag: a bare number exits 2 (README §2).
    check("register --wait 180s" in script, "bootstrap/connect-register: register --wait 180s missing")
    check("wait --wait 180s" in script, "bootstrap/connect-register: wait --wait 180s missing")
    check(
        "register --wait 180s" in script
        and "wait --wait 180s" in script
        and script.index("register --wait 180s") < script.index("wait --wait 180s"),
        "bootstrap/connect-register: wait must follow register in the same container",
    )
    for leaked in ("RAG_SERVICE_TOKEN", "RAG_STATE_ENCRYPTION_KEY"):
        check(leaked not in script, f"bootstrap/connect-register: must not require {leaked}")

    for failure in failures:
        print(f"FAIL {failure}")
    print(f"checks={checks} failures={len(failures)}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
