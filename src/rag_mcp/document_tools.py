"""Client-supplied document content for namespace-confined MCP writes."""

from __future__ import annotations

import base64
import binascii
from dataclasses import dataclass
from pathlib import Path, PureWindowsPath

from mcp import types

from .service_knowledge import MAX_FILE_BYTES, SUPPORTED_EXTENSIONS, KnowledgeDocumentStore, KnowledgeValidationError


@dataclass(frozen=True)
class DocumentRequest:
    name: str
    content: bytes | None = None

    @classmethod
    def from_arguments(cls, arguments, *, writing: bool):
        allowed = {"name", "content", "content_base64"} if writing else {"name"}
        if set(arguments) - allowed:
            raise KnowledgeValidationError("Unknown document arguments")
        name = arguments.get("name")
        if (not isinstance(name, str) or KnowledgeDocumentStore.sanitize_name(name) != name
                or PureWindowsPath(name).is_reserved()):
            raise KnowledgeValidationError("name must be an exact filename, not a path")
        if not writing:
            return cls(name)
        suffix = Path(name).suffix.casefold()
        if suffix not in SUPPORTED_EXTENSIONS:
            raise KnowledgeValidationError("Supported files: .txt, .md, .pdf, .docx, .doc")
        if ("content" in arguments) == ("content_base64" in arguments):
            raise KnowledgeValidationError("Provide exactly one of content or content_base64")
        if "content" in arguments:
            text = arguments["content"]
            if suffix not in {".txt", ".md"}:
                raise KnowledgeValidationError("PDF, DOCX and DOC require content_base64")
            if not isinstance(text, str) or len(text) > MAX_FILE_BYTES:
                raise KnowledgeValidationError("content must be text no larger than 10 MiB")
            try:
                content = text.encode("utf-8")
            except UnicodeEncodeError as exc:
                raise KnowledgeValidationError("content must be valid UTF-8 text") from exc
        else:
            encoded = arguments["content_base64"]
            if not isinstance(encoded, str) or len(encoded) > 4 * ((MAX_FILE_BYTES + 2) // 3):
                raise KnowledgeValidationError("content_base64 must encode a file no larger than 10 MiB")
            try:
                content = base64.b64decode(encoded, validate=True)
            except (ValueError, binascii.Error) as exc:
                raise KnowledgeValidationError("content_base64 is not valid Base64") from exc
        if not content or len(content) > MAX_FILE_BYTES:
            raise KnowledgeValidationError("File must be non-empty and no larger than 10 MiB")
        return cls(name, content)


def document_tool_entries(mutate):
    from .server import RegisteredTool

    entries = []
    descriptions = {
        "add_document": "Add a new file to the connected knowledge base and index it. Rejects existing filenames. Uses recursive chunking. Duplicate content under another name is allowed.",
        "update_document": "Replace the complete content of an existing knowledge file and rebuild its index. Requires the exact existing filename and preserves its chunking settings. This overwrites content; use only when the user requests an update.",
        "delete_document": "Delete an existing knowledge file and its search index by exact filename. Use only when the user requests deletion. This cannot be undone through MCP.",
    }
    for name, description in descriptions.items():
        writing = name != "delete_document"
        properties = {"name": {"type": "string", "minLength": 1, "maxLength": 200,
                               "description": "Exact filename including extension; no directory path."}}
        schema = {"type": "object", "properties": properties, "required": ["name"], "additionalProperties": False}
        if writing:
            description += " Supply content for UTF-8 TXT/Markdown, or content_base64 for file bytes (required for PDF/DOCX/DOC). Maximum decoded size: 10 MiB. No server paths or URLs."
            properties.update(
                content={"type": "string", "minLength": 1, "maxLength": MAX_FILE_BYTES},
                content_base64={"type": "string", "minLength": 1, "maxLength": 4 * ((MAX_FILE_BYTES + 2) // 3)},
            )
            schema["oneOf"] = [{"required": ["content"]}, {"required": ["content_base64"]}]

        async def execute(request, operation=name):
            return await mutate(operation, request)

        def validate(arguments, writing=writing):
            return DocumentRequest.from_arguments(arguments, writing=writing)

        entries.append(RegisteredTool(types.Tool(
            name=name, description=description, input_schema=schema,
            annotations=types.ToolAnnotations(read_only_hint=False, destructive_hint=name != "add_document",
                                              idempotent_hint=False, open_world_hint=False),
        ), validate, execute))
    return entries
