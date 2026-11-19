"""Exception hierarchy shared by the LightRAG sidecar modules.

Every message built in this package is safe to return to callers: it names a
field, a category or a next step and never quotes document text, chunk text,
query text, upstream response bodies or API keys. The Go backend shows the JSON
``detail`` value to end users, so the wording is deliberately generic.

``status_code`` mirrors the HTTP contract: invalid input 400, unauthorized 401,
unknown namespace 404, upstream/engine failure 503, mapping failure 500.
"""

from __future__ import annotations


class ServiceError(RuntimeError):
    """Base class for every handled failure carrying a caller-safe message."""

    status_code = 500


class InputError(ServiceError):
    """A request parameter is malformed or out of range (HTTP 400)."""

    status_code = 400


class RevisionChanged(ServiceError):
    status_code = 409


class UnauthorizedError(ServiceError):
    """The bearer token is missing or does not match (HTTP 401)."""

    status_code = 401


class NotFoundError(ServiceError):
    """The namespace or document is not known to this process (HTTP 404)."""

    status_code = 404


class EngineUnavailableError(ServiceError):
    """LightRAG or an upstream model endpoint failed (HTTP 503)."""

    status_code = 503


class ProcessingError(ServiceError):
    """The engine answered but its result could not be mapped (HTTP 500)."""

    status_code = 500


class ConfigError(ServiceError):
    """The deployment configuration is unusable (refuses startup, reports 503)."""

    status_code = 503
