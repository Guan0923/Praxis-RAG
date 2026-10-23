"""Attach immutable artifact references to an already-executed search."""

from __future__ import annotations

import base64
import copy
import json
import hashlib
import mimetypes
import time
from uuid import uuid4

from sqlalchemy import select

from .result_snapshots import SnapshotUnavailable
from .schema import blobs, upload_objects, SOURCE_BUCKET, PDF_ASSET_BUCKET, WORD_ASSET_BUCKET


class SearchReferences:
    def __init__(self, state):
        self.state = state

    def capture(self, namespace, payload):
        snapshots = self.state.results
        created = time.time()
        expires = created + snapshots.settings.ttl_seconds
        result = copy.deepcopy(payload)
        artifacts = {}
        records = {}
        bytes_used = 0
        count = 0
        registered = {}
        artifact_errors = False

        def register(document_id, version, kind, name, mime, content):
            nonlocal count, bytes_used, artifact_errors
            identity = (document_id, version, kind, name, hashlib.sha256(content).hexdigest())
            if identity in registered:
                return registered[identity]
            if count >= snapshots.settings.max_artifacts_per_search:
                artifact_errors = True
                return None
            if len(content) + bytes_used > snapshots.settings.max_artifact_bytes:
                artifact_errors = True
                return None
            try:
                reference = self.state.artifacts.register(namespace, document_id, version,
                    kind, name, mime, content, expires_at=expires)
            except Exception:
                artifact_errors = True
                return None
            registered[identity] = reference
            count += 1
            bytes_used += len(content)
            artifacts[reference["artifact_id"]] = reference
            return reference

        for hit in result.get("results", []):
            document_id = str(hit.get("document_id", ""))
            if not document_id:
                continue
            version = (hit.get("metadata") or {}).get("upload_version")
            key = document_id, version
            if key not in records:
                references = []
                record = self.state.documents.get_by_id(namespace, document_id)
                if record is not None:
                    name = str(record["name"])
                    actual_version = version or record.get("operation_id")
                    # Search results normally carry their exact version. A legacy
                    # hit must never silently be linked to a newer source version.
                    if version is None and actual_version is not None:
                        records[key] = []
                        continue
                    source = (self.state.uploads.get_object(actual_version, SOURCE_BUCKET, name)
                              if actual_version else self.state.blobs.get(namespace, SOURCE_BUCKET, name))
                    if source is not None:
                        mime = mimetypes.guess_type(name)[0] or "application/octet-stream"
                        ref = register(document_id, actual_version, "source", name, mime, source)
                        if ref:
                            references.append(ref)
                    bucket = (PDF_ASSET_BUCKET if name.lower().endswith(".pdf") else WORD_ASSET_BUCKET
                              if name.lower().endswith((".doc", ".docx")) else None)
                    if bucket:
                        with self.state.engine.connect() as conn:
                            if actual_version:
                                rows = conn.execute(select(upload_objects.c.key, upload_objects.c.content).where(
                                    upload_objects.c.operation_id == actual_version,
                                    upload_objects.c.bucket == bucket,
                                    upload_objects.c.key.in_([f"{document_id}/{name}" for name in
                                        ("document.md", "manifest.json", "document.json", "ocr.json")]))).all()
                            else:
                                rows = conn.execute(select(blobs.c.key, blobs.c.content).where(
                                    blobs.c.namespace == namespace, blobs.c.bucket == bucket,
                                    blobs.c.document_id == document_id,
                                    blobs.c.key.in_([f"{document_id}/{name}" for name in
                                        ("document.md", "manifest.json", "document.json", "ocr.json")]))).all()
                        for artifact_key, content in rows:
                            short_name = artifact_key.rsplit("/", 1)[-1]
                            # Images are registered from the actual public hit below.
                            if short_name not in {"document.md", "manifest.json", "document.json", "ocr.json"}:
                                continue
                            mime = "application/json" if short_name.endswith(".json") else "text/markdown"
                            kind = {"document.md": "markdown", "manifest.json": "manifest",
                                    "document.json": "document", "ocr.json": "ocr"}[short_name]
                            ref = register(document_id, actual_version, kind, short_name, mime, bytes(content))
                            if ref:
                                references.append(ref)
                records[key] = references
            hit["artifacts"] = records[key]
            asset = hit.get("asset")
            if isinstance(asset, dict):
                if asset.get("type") == "image" and isinstance(asset.get("data"), str):
                    content = base64.b64decode(asset["data"], validate=True)
                    ref = register(document_id, version, "image", str((hit.get("metadata") or {}).get(
                        "asset_id", "image"))[:512], asset.get("mime_type") or "image/png", content)
                elif asset.get("type") == "table":
                    ref = register(document_id, version, "table", str((hit.get("metadata") or {}).get(
                        "asset_id", "table"))[:512], "application/json", json.dumps(asset, ensure_ascii=False).encode())
                else:
                    ref = None
                if ref:
                    asset["artifact_id"] = ref["artifact_id"]
                    asset["resource_uri"] = ref["resource_uri"]
                    asset["expires_at"] = ref["expires_at"]
        result["artifacts"] = list(artifacts.values())
        result["artifact_limits"] = {"max_count": snapshots.settings.max_artifacts_per_search,
                                     "max_bytes": snapshots.settings.max_artifact_bytes}
        if artifact_errors:
            result["artifact_warning"] = "Some artifact references could not be stored"
        try:
            return snapshots.save(namespace, result, result_id=str(uuid4()), created_at=created)
        except SnapshotUnavailable:
            return {**result, "snapshot_available": False,
                    "snapshot_error": "Search result snapshot unavailable; original results are returned"}
