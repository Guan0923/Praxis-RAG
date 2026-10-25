"""Full-request MQ envelopes with transient Redis outcomes, never a SQL outbox."""

from __future__ import annotations

import hashlib
import json
import logging
import math
import os
import time
from dataclasses import dataclass
from uuid import uuid4

from .result_snapshots import SnapshotUnavailable, canonical_uuid

_ACTIVE = ("queued", "processing")
_UNAVAILABLE = "Search result expired or unavailable"
MAX_SEARCH_ARGUMENT_BYTES = 1024 * 1024
MAX_SEARCH_ENVELOPE_BYTES = MAX_SEARCH_ARGUMENT_BYTES + 4096
logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class SearchTaskSettings:
    timeout_seconds: float = 120

    def __post_init__(self):
        value = self.timeout_seconds
        if type(value) not in (int, float) or not math.isfinite(value) or value <= 0:
            raise ValueError("Search task timeout_seconds must be finite and positive")

    @classmethod
    def from_env(cls):
        return cls(timeout_seconds=float(os.getenv("RAG_SEARCH_TIMEOUT_SECONDS", "120")))


def validate_result_id(value):
    try:
        return canonical_uuid(value)
    except (ValueError, TypeError, AttributeError):
        raise ValueError("result_id must be a canonical UUID") from None


def validate_search_envelope(payload):
    try:
        if (not isinstance(payload, dict) or type(payload.get("schema")) is not int
                or payload["schema"] != 2
                or set(payload) != {"schema", "result_id", "namespace", "arguments",
                                    "created_at", "expires_at", "deadline"}):
            raise ValueError
        validate_result_id(payload["result_id"])
        if not isinstance(payload["namespace"], str) or not 1 <= len(payload["namespace"]) <= 64:
            raise ValueError
        if not isinstance(payload["arguments"], dict):
            raise ValueError  # noqa: TRY004 - malformed transport data is a validation failure
        times = [payload[key] for key in ("created_at", "expires_at", "deadline")]
        if any(type(value) not in (int, float) or not math.isfinite(value) for value in times):
            raise ValueError
        if not times[0] < times[2] <= times[1]:
            raise ValueError
        arguments = json.dumps(payload["arguments"], ensure_ascii=False, allow_nan=False,
                               separators=(",", ":"))
        # Match the transport's ASCII escaping: Unicode can expand sixfold on wire.
        encoded = json.dumps(payload, ensure_ascii=True, allow_nan=False, separators=(",", ":"))
        if (len(arguments.encode()) > MAX_SEARCH_ARGUMENT_BYTES
                or len(encoded.encode()) > MAX_SEARCH_ENVELOPE_BYTES):
            raise ValueError
        return json.loads(encoded)
    except (ValueError, TypeError, KeyError, RecursionError, OverflowError):
        raise ValueError("Invalid search message envelope") from None


def _request_digest(envelope):
    request = {key: envelope[key] for key in ("namespace", "arguments", "created_at", "expires_at", "deadline")}
    content = json.dumps(request, ensure_ascii=True, allow_nan=False, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(content.encode()).hexdigest()


class SearchTasks:
    def __init__(self, state, settings=None, clock=time.time):
        self.state = state
        self.settings = settings or SearchTaskSettings.from_env()
        self.clock = clock

    @property
    def transient(self):
        from .transient import TransientStore
        return TransientStore(self.state)

    def submit(self, namespace, arguments_dict):
        now = self.clock()
        expires = now + self.state.results.settings.ttl_seconds
        envelope = validate_search_envelope({
            "schema": 2, "result_id": str(uuid4()), "namespace": namespace,
            "arguments": arguments_dict, "created_at": now, "expires_at": expires,
            "deadline": min(now + self.settings.timeout_seconds, expires)})
        record = {key: value for key, value in envelope.items() if key not in ("arguments", "schema")}
        record.update(status="queued", stage="queued", updated_at=now,
                      request_digest=_request_digest(envelope))
        if not self.transient.put("search-result", envelope["result_id"], record,
                                  ttl_seconds=self.state.results.settings.ttl_seconds, nx=True):
            raise RuntimeError("Search admission unavailable")
        return envelope

    def _read(self, namespace, result_id):
        row = self.transient.get("search-result", result_id)
        return row if row is not None and row.get("namespace") == namespace else None

    def _update(self, envelope, callback):
        def change(row):
            if (row is None or row.get("namespace") != envelope["namespace"]
                    or row.get("status") not in _ACTIVE):
                return None
            if "arguments" in envelope and row.get("request_digest") != _request_digest(envelope):
                return None
            if row["expires_at"] <= self.clock() or row["deadline"] <= self.clock():
                return {**row, "status": "timed_out", "stage": "failed", "updated_at": self.clock()}
            return callback(row)
        updated = self.transient.update("search-result", envelope["result_id"], change,
                                        ttl_seconds=self.state.results.settings.ttl_seconds)
        if updated is not None:
            logger.info("Search state result_id=%s status=%s stage=%s",
                        envelope["result_id"], updated["status"], updated["stage"])
        return updated

    def get(self, namespace, result_id, page=1, page_size=10):
        try:
            validate_result_id(result_id)
        except ValueError:
            raise SnapshotUnavailable(_UNAVAILABLE) from None
        try:
            row = self._read(namespace, result_id)
            if row is None:
                return None
            if row.get("status") in _ACTIVE:
                row = self._update({"namespace": namespace, "result_id": result_id}, lambda row: None) or row
        except RuntimeError:
            raise SnapshotUnavailable(_UNAVAILABLE) from None
        if row["status"] == "succeeded":
            return self.state.results.get(namespace, result_id, page=page, page_size=page_size)
        if row["expires_at"] <= self.clock():
            raise SnapshotUnavailable(_UNAVAILABLE)
        if row["status"] not in _ACTIVE:
            message = ("Search timed out before completion; submit a new search"
                       if row["status"] == "timed_out" else "Search failed; submit a new search")
            raise SnapshotUnavailable(message)
        return {"result_id": result_id, "status": "queued", "stage": row["stage"]}

    def claim(self, envelope):
        envelope = validate_search_envelope(envelope)
        row = self._update(envelope, lambda row: {**row, "status": "processing",
                                                 "stage": "processing", "updated_at": self.clock()})
        if (row is None or row.get("namespace") != envelope["namespace"]
                or row.get("status") not in _ACTIVE or row["expires_at"] <= self.clock()):
            return None
        # The broker request must match the admitted metadata, not merely its UUID.
        if any(row[key] != envelope[key] for key in ("created_at", "expires_at", "deadline")):
            return None
        return {**row, "arguments": envelope["arguments"],
                "remaining_seconds": max(0, row["deadline"] - self.clock())}

    def complete(self, task, payload):
        prepared = self.state.results.prepare(task["namespace"], payload, result_id=task["result_id"])
        # Status and bytes share one WATCH/MULTI update. ACK can never precede bytes.
        row = self._update(task, lambda row: prepared)
        if row and row.get("namespace") == task["namespace"] and row.get("status") == "succeeded":
            return row["payload"]
        return None

    def fail(self, task, retry=True, error=None):
        # Provider text is intentionally neither stored nor logged.
        return self._update(task, lambda row: {**row, "status": "queued" if retry else "failed",
                                              "stage": "retry_wait" if retry else "failed",
                                              "updated_at": self.clock()})

    def terminal(self, envelope):
        if isinstance(envelope, dict):
            namespace, result_id = envelope["namespace"], validate_result_id(envelope["result_id"])
        else:
            result_id = validate_result_id(envelope)
            row = self.transient.get("search-result", result_id)
            namespace = row["namespace"] if row else None
        row = self._read(namespace, result_id)
        if row is None:
            return True
        row = self._update({"namespace": namespace, "result_id": result_id}, lambda row: None) or row
        return row.get("status") not in _ACTIVE

    def dead_letter(self, envelope):
        if isinstance(envelope, dict):
            envelope = validate_search_envelope(envelope)
        else:
            result_id = validate_result_id(envelope)
            row = self.transient.get("search-result", result_id)
            if row is None:
                return True
            envelope = {"result_id": result_id, "namespace": row["namespace"]}
        self.fail(envelope, retry=False)
        return True

    def expire(self, limit=20):
        return 0

    def dispatch_pending(self, transport, limit=20):
        return 0
