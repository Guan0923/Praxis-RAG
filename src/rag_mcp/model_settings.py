"""Atomic server-side settings and OS-protected model API secrets."""

from __future__ import annotations

import base64
import ctypes
import json
import os
from dataclasses import replace
from pathlib import Path
from uuid import uuid4

from .model_api import ModelAPIError, ModelConnection, normalize_base_url

KINDS = ("embedding", "reranker")


def _dpapi(value: bytes, *, decrypt: bool = False) -> bytes:
    from ctypes import wintypes

    class Blob(ctypes.Structure):
        _fields_ = [("size", wintypes.DWORD), ("data", ctypes.POINTER(ctypes.c_ubyte))]

    buffer = ctypes.create_string_buffer(value)
    source = Blob(len(value), ctypes.cast(buffer, ctypes.POINTER(ctypes.c_ubyte)))
    output = Blob()
    crypt = ctypes.WinDLL("crypt32", use_last_error=True)
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.LocalFree.argtypes = [ctypes.c_void_p]
    kernel.LocalFree.restype = ctypes.c_void_p
    operation = crypt.CryptUnprotectData if decrypt else crypt.CryptProtectData
    operation.argtypes = [ctypes.POINTER(Blob), ctypes.c_void_p, ctypes.POINTER(Blob),
                          ctypes.c_void_p, ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(Blob)]
    operation.restype = wintypes.BOOL
    if not operation(ctypes.byref(source), None, None, None, None, 1, ctypes.byref(output)):
        raise ModelAPIError("无法使用当前系统账号保护或读取 apikey")
    try:
        return ctypes.string_at(output.data, output.size)
    finally:
        kernel.LocalFree(ctypes.cast(output.data, ctypes.c_void_p))


def _protect(value: str) -> str:
    if not value:
        return ""
    if os.name == "nt":
        return "dpapi:" + base64.b64encode(_dpapi(value.encode())).decode("ascii")
    return "private-file:" + value


def _unprotect(value: str) -> str:
    if not value:
        return ""
    if value.startswith("dpapi:") and os.name == "nt":
        return _dpapi(base64.b64decode(value[6:], validate=True), decrypt=True).decode()
    if value.startswith("private-file:") and os.name != "nt":
        return value[len("private-file:"):]
    raise ModelAPIError("apikey 需要由保存它的系统账号读取")


class ModelSettingsStore:
    def __init__(self, root: Path) -> None:
        self.path = root / "model-api-settings.json"

    def load(self, defaults: dict[str, ModelConnection], collection: str) -> tuple[dict[str, ModelConnection], str]:
        if self.path.is_symlink():
            raise ModelAPIError("模型配置文件不能是符号链接")
        if not self.path.exists():
            return defaults, collection
        try:
            if self.path.is_symlink():
                raise ValueError("symlink")
            raw = json.loads(self.path.read_text(encoding="utf-8"))
            saved = {}
            for kind in KINDS:
                item = raw[kind]
                base_url = normalize_base_url(item["base_url"]) if item["base_url"] else ""
                model = item["model"]
                dimension = item.get("vector_size")
                if (not isinstance(model, str) or not isinstance(item["protected_key"], str)
                        or (dimension is not None and (type(dimension) is not int or dimension <= 0))):
                    raise ValueError("invalid settings")
                saved[kind] = ModelConnection(base_url, model, _unprotect(item["protected_key"]), dimension)
            if not isinstance(raw["collection"], str) or not raw["collection"].strip():
                raise ValueError("invalid collection")
            return saved, raw["collection"]
        except (OSError, ValueError, KeyError, TypeError) as exc:
            raise ModelAPIError("模型配置文件无法读取，未回退到其他模型") from exc

    def save(self, connections: dict[str, ModelConnection], collection: str) -> None:
        payload = {kind: {"base_url": item.base_url, "model": item.model,
                          "protected_key": _protect(item.api_key), "vector_size": item.vector_size}
                   for kind, item in connections.items()}
        payload["collection"] = collection
        temporary = self.path.with_name(f".model-api-{uuid4().hex}.tmp")
        try:
            if self.path.is_symlink():
                raise OSError("symlink")
            descriptor = os.open(temporary, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                json.dump(payload, stream, ensure_ascii=False, indent=2)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.path)
        except OSError as exc:
            raise ModelAPIError("模型配置保存失败，仍保留原配置") from exc
        finally:
            temporary.unlink(missing_ok=True)


def draft_connection(current: ModelConnection, values: dict[str, object], *, require_model: bool = True) -> ModelConnection:
    base_url = normalize_base_url(str(values.get("base_url", current.base_url)))
    model = str(values.get("model", current.model)).strip()
    if require_model and not model:
        raise ModelAPIError("请填写 model")
    key = values.get("apikey", current.api_key if base_url == current.base_url else "")
    if not isinstance(key, str) or any(char in key for char in "\r\n"):
        raise ModelAPIError("apikey 格式无效")
    return replace(current, base_url=base_url, model=model, api_key=key, vector_size=None)
