"""Portable credential protection for values stored in the shared database.

``enc:v1`` values use AES-GCM with a key supplied through the environment, so every
service instance can decrypt them. Legacy ``dpapi:`` and ``private-file:`` values are
still readable to allow an in-place migration.
"""

from __future__ import annotations

import base64
import ctypes
import os
from functools import lru_cache

from ..model_api import ModelAPIError

_PREFIX = "enc:v1:"


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


def _legacy_protect(value: str) -> str:
    if os.name == "nt":
        return "dpapi:" + base64.b64encode(_dpapi(value.encode())).decode("ascii")
    return "private-file:" + value


def _legacy_unprotect(value: str) -> str:
    if value.startswith("dpapi:") and os.name == "nt":
        return _dpapi(base64.b64decode(value[6:], validate=True), decrypt=True).decode()
    if value.startswith("private-file:") and os.name != "nt":
        return value[len("private-file:"):]
    raise ModelAPIError("apikey 需要由保存它的系统账号读取")


@lru_cache(maxsize=4)
def _cipher(key_text: str):
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM

    raw = _decode_key(key_text)
    return AESGCM(raw)


def _decode_key(key_text: str) -> bytes:
    value = key_text.strip()
    if value.startswith("base64:"):
        value = value[7:]
    try:
        raw = base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))
    except Exception as exc:  # noqa: BLE001 - surfaced as a configuration error
        raise ModelAPIError("RAG_STATE_ENCRYPTION_KEY 必须是 base64 编码的 32 字节密钥") from exc
    if len(raw) != 32:
        raise ModelAPIError("RAG_STATE_ENCRYPTION_KEY 必须是 base64 编码的 32 字节密钥")
    return raw


def protect(value: str, key: str | None = None) -> str:
    if not value:
        return ""
    key = key or os.getenv("RAG_STATE_ENCRYPTION_KEY", "").strip()
    if not key:
        return _legacy_protect(value)
    cipher = _cipher(key)
    nonce = os.urandom(12)
    encrypted = cipher.encrypt(nonce, value.encode("utf-8"), None)
    return _PREFIX + base64.b64encode(nonce + encrypted).decode("ascii")


def unprotect(value: str, key: str | None = None) -> str:
    if not value:
        return ""
    if value.startswith(_PREFIX):
        key = key or os.getenv("RAG_STATE_ENCRYPTION_KEY", "").strip()
        if not key:
            raise ModelAPIError("读取 apikey 需要设置 RAG_STATE_ENCRYPTION_KEY")
        payload = base64.b64decode(value[len(_PREFIX):], validate=True)
        cipher = _cipher(key)
        return cipher.decrypt(payload[:12], payload[12:], None).decode("utf-8")
    return _legacy_unprotect(value)


def is_portable(value: str) -> bool:
    return not value or value.startswith(_PREFIX)
