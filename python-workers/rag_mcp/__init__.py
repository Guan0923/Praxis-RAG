"""External document parser workers for the Go Praxis-RAG backend.

The Go service invokes these scripts as child processes; they are NOT part of
the application backend and never import or serve the HTTP/MCP APIs.

    python -m rag_mcp.pdf_worker         # Docling PDF parsing + manifest export
    python -m rag_mcp.word_vision_worker # optional Word picture descriptions

The package only contains the parser helpers the workers need, so it keeps the
same ``rag_mcp.<module>`` invocation contract the Go defaults expect.
"""

__version__ = "0.4.0"
