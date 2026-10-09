"""OS-protected credentials and provider connection validation."""

from __future__ import annotations

import base64
import ctypes
import os

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


def draft_connection(current: ModelConnection, values: dict[str, object]) -> ModelConnection:
    base_url = normalize_base_url(str(values.get("base_url", current.base_url)))
    key = values.get("apikey", current.api_key if base_url == current.base_url else "")
    if not isinstance(key, str) or any(char in key for char in "\r\n"):
        raise ModelAPIError("apikey 格式无效")
    return ModelConnection(base_url=base_url, api_key=key)
