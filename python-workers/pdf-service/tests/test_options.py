"""Per-request ``options`` (VLM/OCR overrides) contract.

Everything here runs without docling/torch: the options parser, the OCR engine
mapping (over stub Docling option classes) and the HTTP layer (fake converter or
a monkeypatched ``convert_pdf``) are pure Python. A few tests assert the
security property that the request API key never reaches a log, an error body or
the manifest.
"""

from __future__ import annotations

import io
import json
import zipfile
from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

from conftest import parse_files, parse_options, raw_options
from app import parser as parser_module
from app.main import create_app
from app.parser import (
    DEFAULT_OCR_ENGINE,
    EASYOCR_LANGUAGES,
    OCR_ENGINES,
    TESSERACT_LANGUAGES,
    DoclingConverter,
    FakeConverter,
    OcrSettings,
    ParseOptions,
    ParserConfigError,
    ParserOptionsError,
    VlmSettings,
    _ocr_options,
    _parser_metadata,
    _validate_local_models,
    fake_manifest,
    parse_options_payload,
)

REQUEST_API_KEY = "sk-request-secret-value"
ENV_API_KEY = "sk-env-secret-value"


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


class _RecordingOptions:
    """Stub Docling option class that records the kwargs it was built with."""

    def __init__(self, **kwargs) -> None:
        self.kwargs = kwargs


def _stub_modules(*, only: str | None = None) -> SimpleNamespace:
    """Stub ``_load_docling_modules()`` namespace for the OCR mapping."""
    names = ("EasyOcrOptions", "TesseractOcrOptions", "RapidOcrOptions")
    symbols = {}
    for name in names:
        if only is not None and name != only:
            continue
        symbols[name] = type(name, (_RecordingOptions,), {})
    return SimpleNamespace(**symbols)


def _manifest_of(response) -> tuple[dict, zipfile.ZipFile]:
    assert response.status_code == 200, response.text
    archive = zipfile.ZipFile(io.BytesIO(response.content))
    return json.loads(archive.read("manifest.json")), archive


def _full_payload() -> dict:
    return {
        "vlm": {
            "base_url": "https://vlm.internal:8443/v1",
            "api_key": REQUEST_API_KEY,
            "model": "qwen2.5-vl-72b",
            "timeout": 120,
            "concurrency": 4,
        },
        "ocr": {"engine": "tesseract"},
    }


class _ExplodingConverter:
    """Fails the test if the HTTP layer calls the converter at all."""

    name = "exploding"

    def convert(self, source, output_dir, scratch_dir, *, options=None):
        raise AssertionError("converter must not run for an invalid options payload")


# ---------------------------------------------------------------------------
# parse_options_payload: defaults and backward compatibility
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "raw", [None, "", "   ", "null", "{}", '{"vlm": null, "ocr": null}', '{"ocr": {}}']
)
def test_empty_options_keep_server_configuration(raw) -> None:
    options = parse_options_payload(raw)

    assert isinstance(options, ParseOptions)
    assert options.vlm is None
    assert options.ocr.engine == DEFAULT_OCR_ENGINE == "easyocr"
    assert options.vlm_or(None) is None


def test_vlm_options_allow_an_empty_api_key() -> None:
    """The Go client emits ``api_key:""`` when the endpoint needs no credential."""
    payload = {"vlm": {"base_url": "http://vlm.local/v1", "api_key": "", "model": "qwen-vl"}}

    vlm = parse_options_payload(json.dumps(payload)).vlm

    assert vlm is not None
    assert vlm.api_key == ""
    assert vlm.headers() == {}


def test_empty_vlm_object_falls_back_to_the_environment_client() -> None:
    env_vlm = VlmSettings("http://env.example/v1", ENV_API_KEY, "env-model", 120.0, 4)

    options = parse_options_payload('{"vlm": {}}')

    assert options.vlm is None
    assert options.vlm_or(env_vlm) is env_vlm


def test_full_payload_builds_vlm_and_ocr_settings() -> None:
    options = parse_options_payload(json.dumps(_full_payload()))

    assert options.vlm is not None
    assert options.vlm.base_url == "https://vlm.internal:8443/v1"
    assert options.vlm.chat_url == "https://vlm.internal:8443/v1/chat/completions"
    assert options.vlm.model == "qwen2.5-vl-72b"
    assert options.vlm.api_key == REQUEST_API_KEY
    assert options.vlm.timeout == 120.0
    assert options.vlm.concurrency == 4
    assert options.ocr.engine == "tesseract"


def test_request_vlm_wins_over_the_environment_client() -> None:
    env_vlm = VlmSettings("http://env.example/v1", ENV_API_KEY, "env-model", 120.0, 4)

    resolved = parse_options_payload(json.dumps(_full_payload())).vlm_or(env_vlm)

    assert resolved is not None
    assert resolved.model == "qwen2.5-vl-72b"


def test_ocr_only_payload_keeps_the_environment_vlm() -> None:
    env_vlm = VlmSettings("http://env.example/v1", ENV_API_KEY, "env-model", 120.0, 4)

    options = parse_options_payload('{"ocr": {"engine": "rapidocr"}}')

    assert options.ocr.engine == "rapidocr"
    assert options.vlm_or(env_vlm) is env_vlm


@pytest.mark.parametrize("engine", list(OCR_ENGINES))
def test_every_contract_engine_is_accepted(engine: str) -> None:
    assert parse_options_payload(json.dumps({"ocr": {"engine": engine}})).ocr.engine == engine


@pytest.mark.parametrize(
    ("raw_engine", "expected"),
    [("AUTO", "auto"), (" EasyOCR ", "easyocr"), ("Tesseract", "tesseract"), ("", "easyocr")],
)
def test_engine_ids_are_normalized(raw_engine: str, expected: str) -> None:
    payload = {"ocr": {"engine": raw_engine}}

    assert parse_options_payload(json.dumps(payload)).ocr.engine == expected


def test_vlm_defaults_are_applied_when_timeout_and_concurrency_are_absent() -> None:
    payload = {"vlm": {"base_url": "http://vlm.local/v1", "model": "qwen-vl"}}

    vlm = parse_options_payload(json.dumps(payload)).vlm

    assert vlm is not None
    assert vlm.timeout == 120.0
    assert vlm.concurrency == 4
    assert vlm.api_key == ""


def test_vlm_timeout_keeps_the_environment_floor() -> None:
    payload = {"vlm": {"base_url": "http://vlm.local/v1", "model": "qwen-vl", "timeout": 5}}

    vlm = parse_options_payload(json.dumps(payload)).vlm

    assert vlm is not None
    assert vlm.timeout == 30.0  # DESCRIPTION_TIMEOUT_FLOOR, same as RAG_VLM_TIMEOUT


def test_vlm_concurrency_accepts_an_integral_number() -> None:
    payload = {"vlm": {"base_url": "http://vlm.local/v1", "model": "qwen-vl", "concurrency": 2.0}}

    vlm = parse_options_payload(json.dumps(payload)).vlm

    assert vlm is not None
    assert vlm.concurrency == 2


def test_ocr_settings_normalize_direct_construction() -> None:
    assert OcrSettings(" TESSERACT ").engine == "tesseract"
    assert OcrSettings("").engine == "easyocr"
    assert OcrSettings("auto").effective_engine == "easyocr"
    assert OcrSettings("rapidocr").effective_engine == "rapidocr"


def test_options_payload_accepts_utf8_bytes() -> None:
    options = parse_options_payload(b'{"ocr": {"engine": "rapidocr"}}')

    assert options.ocr.engine == "rapidocr"


@pytest.mark.parametrize("raw", [123, 1.5, True, ["vlm"], {"ocr": {"engine": "easyocr"}}])
def test_options_payload_rejects_non_text_input(raw) -> None:
    with pytest.raises(ParserOptionsError, match="文本字段"):
        parse_options_payload(raw)


def test_invalid_utf8_options_are_rejected_safely() -> None:
    with pytest.raises(ParserOptionsError, match="UTF-8"):
        parse_options_payload(b'{"ocr": {"engine": "\xff"}}')


# ---------------------------------------------------------------------------
# parse_options_payload: rejections (HTTP 422, safe messages)
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "raw",
    [
        "{",  # broken JSON
        "not-json",  # not JSON at all
        "[1, 2]",  # not an object
        '"easyocr"',  # not an object
        "123",  # not an object
        '{"unknown": 1}',  # unknown top-level key
        '{"ocr": "easyocr"}',  # ocr must be an object
        '{"ocr": {"engine": "paddle"}}',  # engine not in the whitelist
        '{"ocr": {"engine": "", "lang": "en"}}',  # unknown nested key
        '{"ocr": {"engine": 3}}',  # engine must be a string
        '{"vlm": "https://vlm.example/v1"}',  # vlm must be an object
        '{"vlm": {"base_url": "http://vlm.example/v1"}}',  # model missing
        '{"vlm": {"model": "qwen-vl"}}',  # base_url missing
        '{"vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "api_key": 7}}',
        '{"vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "timeout": "120"}}',
        '{"vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "timeout": 0}}',
        '{"vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "timeout": true}}',
        '{"vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "timeout": Infinity}}',
        '{"vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "concurrency": 0}}',
        '{"vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "concurrency": 1.5}}',
        '{"vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "concurrency": false}}',
        '{"vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "extra": 1}}',
    ],
)
def test_invalid_options_raise_options_error(raw: str) -> None:
    with pytest.raises(ParserOptionsError):
        parse_options_payload(raw)


def test_invalid_engine_message_is_chinese_and_does_not_echo_the_value() -> None:
    with pytest.raises(ParserOptionsError) as excinfo:
        parse_options_payload('{"ocr": {"engine": "paddleocr"}}')

    message = str(excinfo.value)
    assert "可选值" in message
    assert "paddleocr" not in message


def test_options_errors_never_echo_the_api_key() -> None:
    payload = {
        "vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "api_key": REQUEST_API_KEY},
        "ocr": {"engine": "nope"},
    }

    with pytest.raises(ParserOptionsError) as excinfo:
        parse_options_payload(json.dumps(payload))

    assert REQUEST_API_KEY not in str(excinfo.value)


# ---------------------------------------------------------------------------
# OCR engine -> Docling pipeline options mapping (stub modules)
# ---------------------------------------------------------------------------


def test_easyocr_engine_matches_the_former_worker_pipeline(tmp_path: Path) -> None:
    modules = _stub_modules()
    models = tmp_path / "models"

    result = _ocr_options(modules, OcrSettings("easyocr"), models, use_gpu=True)

    assert isinstance(result, modules.EasyOcrOptions)
    assert result.kwargs == {
        "lang": list(EASYOCR_LANGUAGES),
        "use_gpu": True,
        "download_enabled": False,
        "model_storage_directory": str(models / "EasyOcr"),
        "confidence_threshold": 0.0,
    }


def test_easyocr_without_models_uses_the_docling_cache(tmp_path: Path) -> None:
    result = _ocr_options(_stub_modules(), OcrSettings("easyocr"), None, use_gpu=False)

    assert result.kwargs["model_storage_directory"] is None
    assert result.kwargs["use_gpu"] is False


def test_auto_engine_is_easyocr(tmp_path: Path) -> None:
    modules = _stub_modules()
    models = tmp_path / "models"

    auto = _ocr_options(modules, OcrSettings("auto"), models, use_gpu=False)
    easyocr = _ocr_options(modules, OcrSettings("easyocr"), models, use_gpu=False)

    assert type(auto) is modules.EasyOcrOptions
    assert auto.kwargs == easyocr.kwargs
    assert OcrSettings("auto").effective_engine == "easyocr"


def test_tesseract_engine_maps_the_chinese_english_languages() -> None:
    modules = _stub_modules()

    result = _ocr_options(modules, OcrSettings("tesseract"), models=None, use_gpu=False)

    assert isinstance(result, modules.TesseractOcrOptions)
    assert result.kwargs == {"lang": list(TESSERACT_LANGUAGES)}


def test_rapidocr_engine_uses_the_docling_defaults() -> None:
    modules = _stub_modules()

    result = _ocr_options(modules, OcrSettings("rapidocr"), models=None, use_gpu=False)

    assert isinstance(result, modules.RapidOcrOptions)
    assert result.kwargs == {}


def test_missing_ocr_engine_defaults_to_easyocr() -> None:
    modules = _stub_modules()

    result = _ocr_options(modules, None, models=None, use_gpu=False)

    assert isinstance(result, modules.EasyOcrOptions)


def test_unknown_engine_in_the_mapping_is_a_client_error() -> None:
    with pytest.raises(ParserOptionsError, match="可选值"):
        _ocr_options(_stub_modules(), OcrSettings("paddleocr"), models=None, use_gpu=False)


def test_missing_docling_option_class_is_a_configuration_error() -> None:
    modules = _stub_modules(only="EasyOcrOptions")

    with pytest.raises(ParserConfigError, match="TesseractOcrOptions"):
        _ocr_options(modules, OcrSettings("tesseract"), models=None, use_gpu=False)


def test_ocr_engine_whitelist_matches_the_go_contract() -> None:
    assert OCR_ENGINES == ("auto", "easyocr", "tesseract", "rapidocr")


# ---------------------------------------------------------------------------
# manifest.parser provenance
# ---------------------------------------------------------------------------


def test_parser_metadata_records_the_effective_ocr_engine() -> None:
    metadata = _parser_metadata(None, OcrSettings("rapidocr"))
    assert metadata["ocr_engine"] == "rapidocr"
    assert metadata["formula_model"] == "docling-project/CodeFormulaV2"
    assert metadata["formula_revision"] == parser_module.FORMULA_REVISION

    auto = _parser_metadata(None, OcrSettings("auto"))
    assert auto["ocr_engine"] == "easyocr"


def test_parser_metadata_never_contains_the_api_key() -> None:
    vlm = VlmSettings("https://vlm.internal:8443/v1", REQUEST_API_KEY, "qwen2.5-vl-72b", 120.0, 4)

    metadata = _parser_metadata(vlm, OcrSettings("tesseract"))

    assert metadata["vlm"] == {
        "backend": "remote",
        "model": "qwen2.5-vl-72b",
        "endpoint": "vlm.internal:8443",
    }
    assert metadata["formula_model"] == "qwen2.5-vl-72b"
    assert metadata["formula_revision"] is None
    assert REQUEST_API_KEY not in json.dumps(metadata)


def test_fake_manifest_has_no_ocr_engine() -> None:
    manifest = fake_manifest()

    assert "ocr_engine" in manifest["parser"]
    assert manifest["parser"]["ocr_engine"] is None
    assert manifest["parser"]["converter"] == "fake"


def test_vlm_replaces_the_local_codeformula_requirement() -> None:
    vlm = VlmSettings("http://vlm.example/v1", "", "qwen-vl", 120.0, 4)

    _validate_local_models(None, vlm)  # a request VLM needs no local weights

    with pytest.raises(ParserConfigError, match="Formula recognition is not configured"):
        _validate_local_models(None, None)


# ---------------------------------------------------------------------------
# converter plumbing: request overrides reach convert_pdf
# ---------------------------------------------------------------------------


def _capture_convert_pdf(monkeypatch: pytest.MonkeyPatch) -> dict:
    captured: dict = {}

    def fake_convert(source, output_dir, scratch_dir, *, models=None, vlm=None, ocr=None):
        captured.update(models=models, vlm=vlm, ocr=ocr)
        return fake_manifest()

    monkeypatch.setattr(parser_module, "convert_pdf", fake_convert)
    return captured


def test_docling_converter_uses_request_overrides(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    captured = _capture_convert_pdf(monkeypatch)
    env_vlm = VlmSettings("http://env.example/v1", ENV_API_KEY, "env-model", 120.0, 4)
    converter = DoclingConverter(models_path=tmp_path / "models", vlm=env_vlm)
    options = parse_options_payload(json.dumps(_full_payload()))

    converter.convert(tmp_path / "source.pdf", tmp_path / "out", tmp_path / "scratch", options=options)

    assert captured["models"] == tmp_path / "models"
    assert captured["vlm"] is not None
    assert captured["vlm"].model == "qwen2.5-vl-72b"
    assert captured["vlm"].api_key == REQUEST_API_KEY
    assert captured["ocr"].engine == "tesseract"


def test_docling_converter_falls_back_to_the_environment(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    captured = _capture_convert_pdf(monkeypatch)
    env_vlm = VlmSettings("http://env.example/v1", ENV_API_KEY, "env-model", 120.0, 4)
    converter = DoclingConverter(models_path=None, vlm=env_vlm)

    converter.convert(tmp_path / "source.pdf", tmp_path / "out", tmp_path / "scratch")

    assert captured["vlm"] is env_vlm
    assert captured["ocr"].engine == "easyocr"


def test_docling_converter_accepts_ocr_only_overrides(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    captured = _capture_convert_pdf(monkeypatch)
    env_vlm = VlmSettings("http://env.example/v1", ENV_API_KEY, "env-model", 120.0, 4)
    converter = DoclingConverter(models_path=None, vlm=env_vlm)
    options = parse_options_payload('{"ocr": {"engine": "rapidocr"}}')

    converter.convert(tmp_path / "source.pdf", tmp_path / "out", tmp_path / "scratch", options=options)

    assert captured["vlm"] is env_vlm
    assert captured["ocr"].engine == "rapidocr"


def test_fake_converter_keeps_the_last_options(tmp_path: Path) -> None:
    converter = FakeConverter()
    options = parse_options_payload('{"ocr": {"engine": "tesseract"}}')

    converter.convert(tmp_path / "source.pdf", tmp_path / "out", tmp_path / "scratch", options=options)

    assert converter.last_options is options


# ---------------------------------------------------------------------------
# POST /v1/parse: options over HTTP
# ---------------------------------------------------------------------------


def test_parse_without_options_is_backward_compatible(fake_client: TestClient) -> None:
    response = fake_client.post("/v1/parse", files=parse_files())

    manifest, _ = _manifest_of(response)
    assert manifest["parser"]["converter"] == "fake"
    options = fake_client.app.state.converter.last_options
    assert options.vlm is None
    assert options.ocr.engine == "easyocr"


def test_parse_forwards_options_to_the_converter(fake_client: TestClient) -> None:
    payload = _full_payload()
    payload["ocr"] = {"engine": "rapidocr"}

    response = fake_client.post(
        "/v1/parse", files=parse_files(), data=parse_options(**payload)
    )

    manifest, _ = _manifest_of(response)
    assert manifest["parser"]["converter"] == "fake"  # fake mode ignores options
    options = fake_client.app.state.converter.last_options
    assert options.vlm.model == "qwen2.5-vl-72b"
    assert options.vlm.base_url == "https://vlm.internal:8443/v1"
    assert options.vlm.api_key == REQUEST_API_KEY
    assert options.ocr.engine == "rapidocr"


def test_fake_mode_ignores_options_in_the_manifest(fake_client: TestClient) -> None:
    response = fake_client.post(
        "/v1/parse",
        files=parse_files(),
        data=parse_options(vlm=_full_payload()["vlm"], ocr={"engine": "tesseract"}),
    )

    manifest, _ = _manifest_of(response)
    assert manifest["parser"]["ocr_engine"] is None
    assert manifest["parser"]["vlm"] is None
    assert [block["kind"] for block in manifest["blocks"]] == ["text", "formula", "picture"]


def test_empty_options_text_field_is_backward_compatible(fake_client: TestClient) -> None:
    response = fake_client.post("/v1/parse", files=parse_files(), data=raw_options(""))

    manifest, _ = _manifest_of(response)
    assert response.headers["x-pdf-converter"] == "fake"
    assert fake_client.app.state.converter.last_options.vlm is None
    assert manifest["parser"]["converter"] == "fake"


def test_invalid_options_return_422_before_reading_the_upload() -> None:
    client = TestClient(create_app(converter=_ExplodingConverter()))

    response = client.post(
        "/v1/parse", files=parse_files(), data=raw_options('{"ocr": {"engine": "paddle"}}')
    )

    assert response.status_code == 422
    detail = response.json()["detail"]
    assert isinstance(detail, str)
    assert "可选值" in detail
    assert "paddle" not in detail


def test_broken_json_options_return_422(fake_client: TestClient) -> None:
    response = fake_client.post("/v1/parse", files=parse_files(), data=raw_options("{"))

    assert response.status_code == 422
    assert "JSON" in response.json()["detail"]
    assert fake_client.app.state.converter.last_options is None


def test_invalid_options_never_leak_the_api_key(fake_client: TestClient) -> None:
    payload = {
        "vlm": {"base_url": "http://vlm.example/v1", "model": "qwen-vl", "api_key": REQUEST_API_KEY},
        "ocr": {"engine": "nope"},
    }

    response = fake_client.post(
        "/v1/parse", files=parse_files(), data=parse_options(**payload)
    )

    assert response.status_code == 422
    assert REQUEST_API_KEY not in response.text


def _install_convert_pdf_with_real_metadata(monkeypatch: pytest.MonkeyPatch) -> None:
    """Make the docling path runnable without docling, keeping real provenance."""

    def fake_convert(source, output_dir, scratch_dir, *, models=None, vlm=None, ocr=None):
        manifest = FakeConverter().convert(source, output_dir, scratch_dir)
        manifest["parser"] = parser_module._parser_metadata(vlm, ocr)
        return manifest

    monkeypatch.setattr(parser_module, "convert_pdf", fake_convert)


def test_request_options_drive_the_docling_manifest(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "docling")
    monkeypatch.setenv("RAG_PDF_MODELS_PATH", str(tmp_path / "models"))
    (tmp_path / "models").mkdir()
    _install_convert_pdf_with_real_metadata(monkeypatch)
    client = TestClient(create_app())

    response = client.post(
        "/v1/parse",
        files=parse_files(),
        data=parse_options(
            vlm=_full_payload()["vlm"], ocr={"engine": "rapidocr"}
        ),
    )

    manifest, archive = _manifest_of(response)
    assert response.headers["x-pdf-converter"] == "docling"
    assert manifest["parser"]["ocr_engine"] == "rapidocr"
    assert manifest["parser"]["vlm"]["model"] == "qwen2.5-vl-72b"
    assert manifest["parser"]["vlm"]["endpoint"] == "vlm.internal:8443"
    assert REQUEST_API_KEY not in json.dumps(manifest)
    # The key never reaches any zip entry either (manifest is the only text one).
    for name in archive.namelist():
        assert REQUEST_API_KEY.encode() not in archive.read(name)


def test_auto_engine_is_recorded_as_easyocr_in_the_manifest(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "docling")
    monkeypatch.setenv("RAG_PDF_MODELS_PATH", str(tmp_path / "models"))
    (tmp_path / "models").mkdir()
    _install_convert_pdf_with_real_metadata(monkeypatch)
    client = TestClient(create_app())

    response = client.post(
        "/v1/parse", files=parse_files(), data=parse_options(ocr={"engine": "auto"})
    )

    manifest, _ = _manifest_of(response)
    assert manifest["parser"]["ocr_engine"] == "easyocr"


def test_request_vlm_overrides_a_partial_environment_configuration(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "docling")
    monkeypatch.setenv("RAG_PDF_MODELS_PATH", str(tmp_path / "models"))
    (tmp_path / "models").mkdir()
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")  # RAG_VLM_MODEL missing
    _install_convert_pdf_with_real_metadata(monkeypatch)
    client = TestClient(create_app())

    without_options = client.post("/v1/parse", files=parse_files())
    assert without_options.status_code == 503
    assert "RAG_VLM_MODEL" in without_options.json()["detail"]

    with_options = client.post(
        "/v1/parse", files=parse_files(), data=parse_options(vlm=_full_payload()["vlm"])
    )
    manifest, _ = _manifest_of(with_options)
    assert manifest["parser"]["vlm"]["model"] == "qwen2.5-vl-72b"
    assert REQUEST_API_KEY not in json.dumps(manifest)


def test_partial_environment_vlm_still_blocks_an_ocr_only_request(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "docling")
    monkeypatch.setenv("RAG_PDF_MODELS_PATH", str(tmp_path / "models"))
    (tmp_path / "models").mkdir()
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")  # RAG_VLM_MODEL missing
    _install_convert_pdf_with_real_metadata(monkeypatch)
    client = TestClient(create_app())

    response = client.post(
        "/v1/parse", files=parse_files(), data=parse_options(ocr={"engine": "tesseract"})
    )

    assert response.status_code == 503
    assert "RAG_VLM_MODEL" in response.json()["detail"]
