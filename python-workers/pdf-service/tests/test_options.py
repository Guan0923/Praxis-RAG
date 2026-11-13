"""Per-request ``options`` (vision endpoint overrides) contract.

Everything here runs offline: the options parser is pure Python and the HTTP
layer is exercised with the fake converter or an injected scripted vision
client. A few tests assert the security property that the request API key never
reaches a log, an error body or the manifest.
"""

from __future__ import annotations

import io
import json
import zipfile

import pytest
from fastapi.testclient import TestClient

from conftest import (
    ScriptedVisionClient,
    build_pdf,
    install_scripted_vision,
    parse_files,
    parse_options,
    raw_options,
)
from app.main import create_app
from app.parser import (
    OCR_REMOVED_MESSAGE,
    ParseOptions,
    ParserOptionsError,
    parse_options_payload,
)
from app.vision import VisionSettings

REQUEST_API_KEY = "sk-request-secret-value"
ENV_API_KEY = "sk-env-secret-value"

PAGE_ANSWER = {
    "blocks": [
        {"kind": "text", "text": "Request vision text.", "bbox": {"l": 0.1, "t": 0.1, "r": 0.8, "b": 0.2}}
    ]
}


def _full_payload() -> dict:
    return {
        "vision": {
            "base_url": "https://vlm.internal:8443/v1",
            "api_key": REQUEST_API_KEY,
            "model": "qwen2.5-vl-72b",
            "timeout": 120,
            "concurrency": 4,
        }
    }


def _manifest_of(response) -> tuple[dict, zipfile.ZipFile]:
    assert response.status_code == 200, response.text
    archive = zipfile.ZipFile(io.BytesIO(response.content))
    return json.loads(archive.read("manifest.json")), archive


class _ExplodingConverter:
    """Fails the test if the HTTP layer calls the converter at all."""

    name = "exploding"

    def convert(self, source, output_dir, scratch_dir, *, options=None):
        raise AssertionError("converter must not run for an invalid options payload")


# ---------------------------------------------------------------------------
# parse_options_payload: defaults and compatibility
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "raw",
    [
        None,
        "",
        "   ",
        "null",
        "{}",
        '{"vision": null}',
        '{"vision": {}}',
        '{"vlm": null}',
        '{"vlm": {}}',
    ],
)
def test_empty_options_keep_server_configuration(raw) -> None:
    options = parse_options_payload(raw)
    env_vlm = VisionSettings("http://env.example/v1", ENV_API_KEY, "env-model", 120.0, 4)

    assert isinstance(options, ParseOptions)
    assert options.vision is None
    assert options.vision_or(env_vlm) is env_vlm
    assert options.vision_or(None) is None


def test_full_payload_builds_the_vision_settings() -> None:
    options = parse_options_payload(json.dumps(_full_payload()))

    assert options.vision is not None
    assert options.vision.base_url == "https://vlm.internal:8443/v1"
    assert options.vision.chat_url == "https://vlm.internal:8443/v1/chat/completions"
    assert options.vision.model == "qwen2.5-vl-72b"
    assert options.vision.api_key == REQUEST_API_KEY
    assert options.vision.timeout == 120.0
    assert options.vision.concurrency == 4


def test_legacy_vlm_alias_is_equivalent_to_vision() -> None:
    vision = parse_options_payload(json.dumps(_full_payload())).vision
    legacy = parse_options_payload(json.dumps({"vlm": _full_payload()["vision"]})).vision

    assert legacy is not None
    assert legacy == vision
    assert legacy.chat_url == "https://vlm.internal:8443/v1/chat/completions"


def test_vision_and_vlm_together_are_rejected() -> None:
    payload = {"vision": _full_payload()["vision"], "vlm": _full_payload()["vision"]}

    with pytest.raises(ParserOptionsError) as excinfo:
        parse_options_payload(json.dumps(payload))

    message = str(excinfo.value)
    assert "vision" in message and "vlm" in message
    assert REQUEST_API_KEY not in message


def test_ocr_options_are_rejected_with_the_documented_message() -> None:
    for payload in ('{"ocr": {"engine": "easyocr"}}', '{"ocr": {}}', '{"ocr": null}'):
        with pytest.raises(ParserOptionsError) as excinfo:
            parse_options_payload(payload)
        assert str(excinfo.value) == OCR_REMOVED_MESSAGE


def test_vlm_options_allow_an_empty_api_key() -> None:
    """The Go client emits ``api_key:""`` when the endpoint needs no credential."""
    payload = {"vision": {"base_url": "http://vlm.local/v1", "api_key": "", "model": "qwen-vl"}}

    vision = parse_options_payload(json.dumps(payload)).vision

    assert vision is not None
    assert vision.api_key == ""
    assert vision.headers() == {}


def test_vision_defaults_are_applied_when_timeout_and_concurrency_are_absent() -> None:
    payload = {"vision": {"base_url": "http://vlm.local/v1", "model": "qwen-vl"}}

    vision = parse_options_payload(json.dumps(payload)).vision

    assert vision is not None
    assert vision.timeout == 120.0
    assert vision.concurrency == 4
    assert vision.api_key == ""


def test_vision_timeout_keeps_the_floor() -> None:
    payload = {"vision": {"base_url": "http://vlm.local/v1", "model": "qwen-vl", "timeout": 5}}

    vision = parse_options_payload(json.dumps(payload)).vision

    assert vision is not None
    assert vision.timeout == 30.0  # the 30s floor, same as RAG_VLM_TIMEOUT


def test_vision_concurrency_accepts_an_integral_number() -> None:
    payload = {"vision": {"base_url": "http://vlm.local/v1", "model": "qwen-vl", "concurrency": 2.0}}

    vision = parse_options_payload(json.dumps(payload)).vision

    assert vision is not None
    assert vision.concurrency == 2


def test_options_payload_accepts_utf8_bytes() -> None:
    options = parse_options_payload(b'{"vision": {"base_url": "http://vlm.local/v1", "model": "x"}}')

    assert options.vision is not None
    assert options.vision.model == "x"


@pytest.mark.parametrize("raw", [123, 1.5, True, ["vision"], {"vision": {"base_url": "x", "model": "y"}}])
def test_options_payload_rejects_non_text_input(raw) -> None:
    with pytest.raises(ParserOptionsError, match="文本字段"):
        parse_options_payload(raw)


def test_invalid_utf8_options_are_rejected_safely() -> None:
    with pytest.raises(ParserOptionsError, match="UTF-8"):
        parse_options_payload(b'{"vision": {"model": "\xff"}}')


# ---------------------------------------------------------------------------
# parse_options_payload: rejections (HTTP 422, safe messages)
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "raw",
    [
        "{",  # broken JSON
        "not-json",  # not JSON at all
        "[1, 2]",  # not an object
        '"qwen-vl"',  # not an object
        "123",  # not an object
        '{"unknown": 1}',  # unknown top-level key
        '{"vision": "https://vlm.example/v1"}',  # vision must be an object
        '{"vision": {"base_url": "http://vlm.example/v1"}}',  # model missing
        '{"vision": {"model": "qwen-vl"}}',  # base_url missing
        '{"vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "api_key": 7}}',
        '{"vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "timeout": "120"}}',
        '{"vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "timeout": 0}}',
        '{"vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "timeout": true}}',
        '{"vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "timeout": Infinity}}',
        '{"vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "concurrency": 0}}',
        '{"vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "concurrency": 1.5}}',
        '{"vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "concurrency": false}}',
        '{"vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "extra": 1}}',
    ],
)
def test_invalid_options_raise_options_error(raw: str) -> None:
    with pytest.raises(ParserOptionsError):
        parse_options_payload(raw)


def test_options_errors_never_echo_the_api_key() -> None:
    payload = {
        "vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "api_key": REQUEST_API_KEY},
        "ocr": {"engine": "nope"},
    }

    with pytest.raises(ParserOptionsError) as excinfo:
        parse_options_payload(json.dumps(payload))

    assert REQUEST_API_KEY not in str(excinfo.value)


def test_options_errors_never_echo_the_offending_value() -> None:
    with pytest.raises(ParserOptionsError) as excinfo:
        parse_options_payload('{"vision": {"base_url": "http://vlm.example/v1", "model": 42}}')

    assert "42" not in str(excinfo.value)


# ---------------------------------------------------------------------------
# POST /v1/parse: options over HTTP (fake converter)
# ---------------------------------------------------------------------------


def test_parse_without_options_is_backward_compatible(fake_client: TestClient) -> None:
    response = fake_client.post("/v1/parse", files=parse_files())

    manifest, _ = _manifest_of(response)
    assert manifest["parser"]["converter"] == "fake"
    options = fake_client.app.state.converter.last_options
    assert options.vision is None


def test_parse_forwards_options_to_the_converter(fake_client: TestClient) -> None:
    response = fake_client.post(
        "/v1/parse", files=parse_files(), data=parse_options(**_full_payload())
    )

    manifest, _ = _manifest_of(response)
    assert manifest["parser"]["converter"] == "fake"  # fake mode ignores options
    options = fake_client.app.state.converter.last_options
    assert options.vision.model == "qwen2.5-vl-72b"
    assert options.vision.base_url == "https://vlm.internal:8443/v1"
    assert options.vision.api_key == REQUEST_API_KEY


def test_fake_mode_ignores_options_in_the_manifest(fake_client: TestClient) -> None:
    response = fake_client.post(
        "/v1/parse",
        files=parse_files(),
        data=parse_options(**_full_payload()),
    )

    manifest, _ = _manifest_of(response)
    assert manifest["parser"]["model"] is None
    assert manifest["parser"]["endpoint"] is None
    assert REQUEST_API_KEY not in response.text
    assert [block["kind"] for block in manifest["blocks"]] == ["text", "formula", "picture"]


def test_empty_options_text_field_is_backward_compatible(fake_client: TestClient) -> None:
    response = fake_client.post("/v1/parse", files=parse_files(), data=raw_options(""))

    manifest, _ = _manifest_of(response)
    assert response.headers["x-pdf-converter"] == "fake"
    assert fake_client.app.state.converter.last_options.vision is None
    assert manifest["parser"]["converter"] == "fake"


def test_invalid_options_return_422_before_reading_the_upload() -> None:
    client = TestClient(create_app(converter=_ExplodingConverter()))

    response = client.post("/v1/parse", files=parse_files(), data=raw_options("{"))

    assert response.status_code == 422
    assert isinstance(response.json()["detail"], str)


def test_broken_json_options_return_422(fake_client: TestClient) -> None:
    response = fake_client.post("/v1/parse", files=parse_files(), data=raw_options("{"))

    assert response.status_code == 422
    assert "JSON" in response.json()["detail"]
    assert fake_client.app.state.converter.last_options is None


def test_ocr_options_return_422_over_http(fake_client: TestClient) -> None:
    response = fake_client.post(
        "/v1/parse",
        files=parse_files(),
        data=raw_options('{"ocr": {"engine": "easyocr"}}'),
    )

    assert response.status_code == 422
    assert response.json()["detail"] == OCR_REMOVED_MESSAGE


def test_invalid_options_never_leak_the_api_key(fake_client: TestClient) -> None:
    payload = {
        "vision": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "api_key": REQUEST_API_KEY},
        "ocr": {"engine": "nope"},
    }

    response = fake_client.post("/v1/parse", files=parse_files(), data=parse_options(**payload))

    assert response.status_code == 422
    assert REQUEST_API_KEY not in response.text


# ---------------------------------------------------------------------------
# Request options drive the real vision converter
# ---------------------------------------------------------------------------


def test_request_options_drive_the_vision_manifest(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "vision")
    scripted = ScriptedVisionClient(page_answers=[PAGE_ANSWER])
    install_scripted_vision(monkeypatch, scripted)
    client = TestClient(create_app())

    response = client.post(
        "/v1/parse",
        files=parse_files(build_pdf(pages=1)),
        data=parse_options(**_full_payload()),
    )

    manifest, archive = _manifest_of(response)
    assert response.headers["x-pdf-converter"] == "vision"
    assert manifest["parser"]["converter"] == "vision"
    assert manifest["parser"]["model"] == "qwen2.5-vl-72b"
    assert manifest["parser"]["endpoint"] == "vlm.internal:8443"
    assert [block["kind"] for block in manifest["blocks"]] == ["text"]
    assert REQUEST_API_KEY not in json.dumps(manifest)
    # The key never reaches any zip entry either (manifest is the only text one).
    for name in archive.namelist():
        assert REQUEST_API_KEY.encode() not in archive.read(name)


def test_request_vision_overrides_a_partial_environment_configuration(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "vision")
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")  # RAG_VLM_MODEL missing
    scripted = ScriptedVisionClient(page_answers=[PAGE_ANSWER])
    install_scripted_vision(monkeypatch, scripted)
    client = TestClient(create_app())

    without_options = client.post("/v1/parse", files=parse_files(build_pdf(pages=1)))
    assert without_options.status_code == 503
    assert "RAG_VLM_MODEL" in without_options.json()["detail"]

    with_options = client.post(
        "/v1/parse",
        files=parse_files(build_pdf(pages=1)),
        data=parse_options(**_full_payload()),
    )
    manifest, _ = _manifest_of(with_options)
    assert manifest["parser"]["model"] == "qwen2.5-vl-72b"
    assert REQUEST_API_KEY not in json.dumps(manifest)
