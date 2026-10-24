"""Dual-era HTTP endpoints with namespace-owned sessions and subscriptions."""

from __future__ import annotations

import asyncio
import json
import logging
from contextlib import asynccontextmanager
from dataclasses import asdict
from functools import partial
from urllib.parse import parse_qs, quote, unquote, urlencode, urlsplit

import anyio
from fastapi import HTTPException
from mcp import types
from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser
from mcp.server.auth.provider import AccessToken
from mcp.server.streamable_http_manager import StreamableHTTPSessionManager
from mcp.server.transport_security import TransportSecurityMiddleware
from mcp.server.subscriptions import (
    InMemorySubscriptionBus,
    ListenHandler,
    PromptsListChanged,
    ResourcesListChanged,
    ResourceUpdated,
    ToolsListChanged,
)
from mcp.shared.exceptions import MCPError
from starlette.requests import Request
from starlette.responses import JSONResponse

from .catalog import CatalogStore
from .document_readers import DocumentReadError
from .resources import document_uri, parse_uri, resource_page
from .server import create_registry, create_server, validate_artifact_range, validate_uuid
from .service_knowledge import MAX_FILE_BYTES, KnowledgeError, KnowledgeValidationError
from .state.uploads import UploadConflict

logger = logging.getLogger(__name__)


def _paginate(items, params):
    cursor = params.cursor if params else None
    if cursor is not None and (not cursor.isdigit() or len(cursor) > 9):
        raise ValueError("Invalid list cursor")
    start = int(cursor or 0)
    if start > len(items):
        raise ValueError("List cursor is out of range")
    return items[start : start + 50], str(start + 50) if start + 50 < len(items) else None


def _artifact_uri(namespace, artifact_id):
    return f"rag://artifacts/{quote(namespace, safe='')}/{artifact_id}"


def _parse_artifact_uri(namespace, uri):
    if any(ord(char) <= 32 or ord(char) == 127 for char in uri) or "#" in uri:
        raise ValueError("Invalid artifact URI")
    parsed = urlsplit(uri)
    parts = parsed.path.split("/")
    if parsed.scheme != "rag" or parsed.netloc != "artifacts" or parsed.fragment or len(parts) != 3:
        raise ValueError("Invalid artifact URI")
    uri_namespace, raw_id = (unquote(part, errors="strict") for part in parts[1:])
    artifact_id = validate_uuid(raw_id)
    if uri_namespace != namespace or f"rag://artifacts{parsed.path}" != _artifact_uri(namespace, artifact_id):
        raise ValueError("Artifact URI is outside this namespace or non-canonical")
    query = parse_qs(parsed.query, keep_blank_values=True, strict_parsing=True)
    if (set(query) - {"offset", "length"}
            or any(len(values) != 1 for values in query.values())
            or ("?" in uri and not parsed.query)
            or "&".join(f"{key}={values[0]}" for key, values in query.items()) != parsed.query):
        raise ValueError("Invalid artifact chunk parameters")
    values = []
    for key, default in (("offset", "0"), ("length", "65536")):
        raw = query.get(key, [default])[0]
        if not raw or not raw.isascii() or not raw.isdecimal() or len(raw) > 20:
            raise ValueError("Invalid artifact chunk parameters")
        value = int(raw)
        if raw != str(value):
            raise ValueError("Artifact chunk parameters must be canonical integers")
        values.append(value)
    offset, length = values
    validate_artifact_range(offset, length)
    return artifact_id, offset, length


class NamespaceEndpoint:
    def __init__(self, namespace, runtime, catalog, security, document_mutator):
        self.namespace = namespace
        self.runtime = runtime
        if not callable(getattr(runtime, "get_search_result", None)):
            self.get_search_result = None
        self.catalog = catalog
        self.bus = InMemorySubscriptionBus()
        self.listen = ListenHandler(self.bus)
        self.stop = anyio.Event()
        self.legacy_sessions = {}
        self.legacy_resources: dict[str, set[str]] = {}
        self.server = create_server(
            self,
            catalog,
            document_mutator=document_mutator,
            on_list_prompts=self.list_prompts,
            on_get_prompt=self.get_prompt,
            on_list_resources=self.list_resources,
            on_read_resource=self.read_resource,
            on_list_resource_templates=self.list_templates,
            on_subscribe_resource=self.subscribe,
            on_unsubscribe_resource=self.unsubscribe,
            on_subscriptions_listen=self.subscriptions_listen,
        )
        self.server.middleware.append(self.observe)
        self.manager = StreamableHTTPSessionManager(
            self.server,
            stateless=False,
            json_response=False,
            security_settings=security,
            # JSON may escape one UTF-8 byte as six characters; leave room for the envelope.
            max_request_body_size=6 * MAX_FILE_BYTES + 64 * 1024,
        )

    def search(self, request):
        return self.runtime.search(self.namespace, asdict(request))

    async def search_queued(self, request):
        return await McpHttpEndpoint.search_queued(self, self.namespace, request)

    def get_search_result(self, result_id, *, page=1, page_size=10):
        return self.runtime.get_search_result(self.namespace, result_id, page=page, page_size=page_size)

    async def run(self, *, task_status=anyio.TASK_STATUS_IGNORED):
        # The manager's task group must enter and exit in the same owner task.
        async with self.manager.run():
            task_status.started()
            await self.stop.wait()
            self.listen.close()
            await anyio.lowlevel.checkpoint()
        self.legacy_sessions.clear()
        self.legacy_resources.clear()

    async def observe(self, ctx, call_next):
        try:
            result = await call_next(ctx)
            if ctx.protocol_version != "2026-07-28" and ctx.request is not None:
                session_id = ctx.request.headers.get("mcp-session-id")
                if session_id:
                    self.legacy_sessions[session_id] = ctx.session
            return result
        except (ValueError, KeyError, KnowledgeError, DocumentReadError) as exc:
            raise MCPError(types.INVALID_PARAMS, str(exc)) from exc

    async def list_resources(self, ctx, params):
        documents = await asyncio.to_thread(self.runtime.list_documents, self.namespace)
        resources = [
            types.Resource(
                uri=document_uri(self.namespace, item["name"]),
                name=item["name"],
                mime_type="application/json",
                description="Paragraph-paged document text; follow next_uri to continue.",
            )
            for item in documents
        ]
        items, cursor = _paginate(resources, params)
        return types.ListResourcesResult(resources=items, next_cursor=cursor)

    async def list_templates(self, ctx, params):
        return types.ListResourceTemplatesResult(
            resource_templates=[
                types.ResourceTemplate(
                    uri_template=f"rag://knowledge/{self.namespace}/{{name}}{{?page,version}}",
                    name="Document paragraph pages",
                    mime_type="application/json",
                ),
                types.ResourceTemplate(
                    uri_template="rag://artifacts/{namespace}/{artifact_id}{?offset,length}",
                    name="Artifact byte chunks",
                    description="Read an expiring artifact UUID as a bounded JSON chunk envelope; follow next_uri.",
                    mime_type="application/json",
                ),
            ]
        )

    async def read_resource(self, ctx, params):
        if urlsplit(str(params.uri)).netloc == "artifacts":
            artifact_id, offset, length = _parse_artifact_uri(self.namespace, str(params.uri))
            payload = dict(await asyncio.to_thread(
                self.runtime.get_artifact, self.namespace, artifact_id, offset=offset, length=length,
            ))
            next_offset = payload.get("next_offset")
            payload["next_uri"] = (
                _artifact_uri(self.namespace, artifact_id)
                + "?" + urlencode({"offset": next_offset, "length": length})
                if next_offset is not None else None
            )
            return types.ReadResourceResult(contents=[types.TextResourceContents(
                uri=params.uri, mime_type="application/json", text=json.dumps(payload, ensure_ascii=False),
            )])
        name, page, expected = parse_uri(self.namespace, params.uri)
        text, version = await asyncio.to_thread(self.runtime.read_document, self.namespace, name)
        payload = resource_page(self.namespace, name, text, version, page, expected)
        return types.ReadResourceResult(
            contents=[
                types.TextResourceContents(
                    uri=params.uri,
                    mime_type="application/json",
                    text=json.dumps(payload, ensure_ascii=False),
                )
            ]
        )

    async def list_prompts(self, ctx, params):
        data = await asyncio.to_thread(self.catalog.snapshot)
        prompts = [
            types.Prompt(
                name=name,
                description=prompt.description,
                arguments=[types.PromptArgument(**argument.model_dump()) for argument in prompt.arguments],
            )
            for name, prompt in sorted(data.prompts.items())
        ]
        items, cursor = _paginate(prompts, params)
        return types.ListPromptsResult(prompts=items, next_cursor=cursor)

    async def get_prompt(self, ctx, params):
        data = await asyncio.to_thread(self.catalog.snapshot)
        if params.name not in data.prompts:
            raise ValueError("Unknown prompt")
        prompt = data.prompts[params.name]
        return types.GetPromptResult(
            description=prompt.description,
            messages=[
                types.PromptMessage(
                    role=message["role"],
                    content=types.TextContent(type="text", text=message["text"]),
                )
                for message in prompt.render(params.arguments or {})
            ],
        )

    async def validate_subscription(self, uri):
        name, _, _ = parse_uri(self.namespace, uri)
        if uri != document_uri(self.namespace, name):
            raise ValueError("Subscribe to the document URI, not a page URI")
        documents = await asyncio.to_thread(self.runtime.list_documents, self.namespace)
        if name not in {item["name"] for item in documents}:
            raise ValueError("Document does not exist")

    async def subscriptions_listen(self, ctx, params):
        for uri in params.notifications.resource_subscriptions or []:
            await self.validate_subscription(uri)
        return await self.listen(ctx, params)

    async def subscribe(self, ctx, params):
        await self.validate_subscription(params.uri)
        session_id = ctx.request.headers["mcp-session-id"]
        self.legacy_resources.setdefault(session_id, set()).add(params.uri)
        return types.EmptyResult()

    async def unsubscribe(self, ctx, params):
        session_id = ctx.request.headers["mcp-session-id"]
        self.legacy_resources.get(session_id, set()).discard(params.uri)
        return types.EmptyResult()

    def forget(self, session_id):
        self.legacy_sessions.pop(session_id, None)
        self.legacy_resources.pop(session_id, None)

    async def publish(self, event):
        await self.bus.publish(event)
        for session_id, session in list(self.legacy_sessions.items()):
            if isinstance(event, ResourceUpdated) and event.uri not in self.legacy_resources.get(session_id, set()):
                continue
            try:
                with anyio.move_on_after(1) as timeout:
                    if isinstance(event, ToolsListChanged):
                        await session.send_tool_list_changed()
                    elif isinstance(event, PromptsListChanged):
                        await session.send_prompt_list_changed()
                    elif isinstance(event, ResourcesListChanged):
                        await session.send_resource_list_changed()
                    else:
                        await session.send_resource_updated(event.uri)
                if timeout.cancel_called:
                    self.forget(session_id)
            except Exception:
                logger.warning("Dropping unavailable legacy MCP notification connection")
                self.forget(session_id)


class McpHttpEndpoint:
    def __init__(self, runtime, tenant, security, state):
        self.runtime = runtime
        self._tenant = tenant
        self.security = security
        self._security = TransportSecurityMiddleware(security)
        self.catalogs = CatalogStore(state)
        self.state = state
        self.peers: dict[str, NamespaceEndpoint] = {}
        self._lock = anyio.Lock()
        self._mutations: dict[str, anyio.Lock] = {}
        self._tasks = None

    @asynccontextmanager
    async def run(self):
        async with anyio.create_task_group() as tasks:
            self._tasks = tasks
            try:
                yield
            finally:
                self._tasks = None
                for peer in self.peers.values():
                    peer.stop.set()
        self.peers.clear()

    def mutation_lock(self, namespace):
        return self._mutations.setdefault(namespace, anyio.Lock())

    def registry(self, namespace):
        class Executor:
            def search(inner, request):
                return self.runtime.search(namespace, asdict(request))

            async def search_queued(inner, request):
                return await self.search_queued(namespace, request)

            def get_search_result(inner, result_id, *, page=1, page_size=10):
                return self.runtime.get_search_result(namespace, result_id, page=page, page_size=page_size)

        executor = Executor()
        if not callable(getattr(self.runtime, "get_search_result", None)):
            executor.get_search_result = None
        return create_registry(executor, self.catalogs.for_namespace(namespace), partial(self.mutate_document, namespace))

    async def search_queued(self, namespace, request):
        # Submission is durable even if the client disconnects during admission.
        with anyio.CancelScope(shield=True):
            submitted = await anyio.to_thread.run_sync(
                self.runtime.submit_search, namespace, asdict(request.search))
        result_id = submitted["result_id"]
        if request.wait_seconds > 0:
            with anyio.move_on_after(request.wait_seconds):
                while True:
                    payload = await anyio.to_thread.run_sync(
                        self.runtime.get_search_result, namespace, result_id, abandon_on_cancel=True)
                    if payload.get("status") != "queued":
                        return payload
                    await anyio.sleep(0.1)
        return {"result_id": result_id}

    async def mutate_document(self, namespace, operation, request):
        if operation in ("add_document", "get_document_status"):
            try:
                with anyio.CancelScope(shield=True):
                    if operation == "get_document_status":
                        return await anyio.to_thread.run_sync(
                            self.runtime.get_document_status, namespace, request.artifact_id)
                    return await anyio.to_thread.run_sync(partial(
                        self.runtime.add_document, namespace, request.name, request.content,
                        **({"idempotency_key": request.idempotency_key} if request.idempotency_key else {}),
                    ))
            except UploadConflict as exc:
                raise KnowledgeValidationError(
                    "Document conflicts with an existing filename, queued upload or idempotency key."
                ) from exc
            except KnowledgeError:
                raise
            except Exception as exc:
                logger.warning("MCP document submission/status failed (%s)", type(exc).__name__)
                raise KnowledgeError("Document operation unavailable") from exc
        async with self.mutation_lock(namespace):
            # Finish the storage change and notification together even if the caller disconnects.
            with anyio.CancelScope(shield=True):
                try:
                    before = await anyio.to_thread.run_sync(
                        getattr(self.runtime, "authoritative_documents", self.runtime.list_documents), namespace)
                    if operation == "delete_document":
                        if request.name not in {item["name"] for item in before}:
                            raise KnowledgeValidationError(f"Document does not exist: {request.name}")
                        result = await anyio.to_thread.run_sync(self.runtime.delete, namespace, request.name)
                        result.update(status="deleted", document=result.pop("deleted"))
                    else:
                        write = partial(
                            self.runtime.write_document, namespace, request.name, request.content,
                            overwrite=operation == "update_document",
                        )
                        if request.idempotency_key is not None:
                            write = partial(write, idempotency_key=request.idempotency_key)
                        result = await anyio.to_thread.run_sync(write)
                    await self.documents_changed(namespace, before, result.pop("documents"))
                    return {**result, "uri": document_uri(namespace, request.name)}
                except UploadConflict as exc:
                    raise KnowledgeValidationError(
                        "Upload conflicts with another operation or idempotency key."
                    ) from exc
                except KnowledgeError:
                    raise
                except Exception as exc:
                    logger.exception("MCP document mutation failed")
                    raise KnowledgeError(f"Document operation failed: {type(exc).__name__}") from exc

    async def publish(self, namespace, event):
        peer = self.peers.get(namespace)
        if peer:
            await peer.publish(event)

    async def documents_changed(self, namespace, before, after):
        old = {item["name"]: item for item in before}
        new = {item["name"]: item for item in after}
        if old == new:
            return
        await self.publish(namespace, ResourcesListChanged())
        for name, record in old.items():
            if new.get(name) != record:
                await self.publish(namespace, ResourceUpdated(uri=document_uri(namespace, name)))

    async def __call__(self, scope, receive, send):
        request = Request(scope, receive)
        try:
            if not request.headers.get("authorization"):
                raise HTTPException(status_code=401, detail="MCP 请求必须提供 Authorization: Bearer <API_KEY>",
                                    headers={"WWW-Authenticate": "Bearer"})
            namespace = self._tenant(request.headers.get("authorization"), request.headers.get("x-rag-namespace"), request)
        except HTTPException as exc:
            await JSONResponse({"detail": exc.detail}, status_code=exc.status_code, headers=exc.headers)(scope, receive, send)
            return
        rejection = await self._security.validate_request(request, is_post=request.method == "POST")
        if rejection is not None:
            await rejection(scope, receive, send)
            return
        scope["user"] = AuthenticatedUser(
            AccessToken(
                token=request.headers.get("authorization", ""),
                client_id="rag-service",
                subject=namespace,
                scopes=[],
            )
        )
        async with self._lock:
            peer = self.peers.get(namespace)
            if peer is None:
                if self._tasks is None:
                    await JSONResponse({"detail": "MCP service is not running"}, status_code=503)(scope, receive, send)
                    return
                peer = NamespaceEndpoint(namespace, self.runtime, self.catalogs.for_namespace(namespace), self.security,
                                         partial(self.mutate_document, namespace))
                await self._tasks.start(peer.run)
                self.peers[namespace] = peer

        async def observe_response(message):
            if request.method == "DELETE" and message["type"] == "http.response.start" and message["status"] == 200:
                peer.forget(request.headers.get("mcp-session-id"))
            await send(message)

        await peer.manager.handle_request(scope, receive, observe_response)
