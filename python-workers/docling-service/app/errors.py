"""Exception hierarchy shared by the Docling service modules.

Every message built here is safe to return to callers: it names the offending
field/format and never quotes document text, uploaded bytes or upstream
response bodies. The Go backend shows ``detail`` to users.
"""

from __future__ import annotations


class ParserError(RuntimeError):
    """Base class for parse failures with a caller-safe message."""


class ParserInputError(ParserError):
    """The uploaded file is invalid, empty or over the size limit (HTTP 400)."""


class ParserUnsupportedError(ParserError):
    """The uploaded format is not accepted by this service (HTTP 415)."""


class ParserConfigError(ParserError):
    """The deployment is misconfigured, for example Docling is unavailable."""


class ParserConversionError(ParserError):
    """Docling converted the document but the result could not be mapped."""
