"""Exception hierarchy shared by the PDF service modules.

The messages built by these exceptions are safe to return to callers: they
carry field names, page numbers and configuration variable names, never
document text, request values or vision-model response bodies.
"""

from __future__ import annotations


class ParserError(RuntimeError):
    """Base class for parse failures with a message safe to return to callers."""


class ParserConfigError(ParserError):
    """The deployment is misconfigured (no vision endpoint, invalid settings)."""


class ParserConversionError(ParserError):
    """The document could not be converted into a valid manifest."""


class ParserOptionsError(ParserError):
    """The per-request ``options`` payload is invalid (HTTP 422).

    Messages are Chinese, name the offending field only and never echo request
    values: the Go backend shows ``detail`` to users, and ``options`` may carry
    the vision API key.
    """


class VisionError(ParserConversionError):
    """One OpenAI-compatible vision call failed.

    The message never quotes the upstream response body or the API key; the
    converter rewrites it into a page-specific Chinese error for the caller.
    """
