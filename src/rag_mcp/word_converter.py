"""Isolated legacy Word conversion. Never automate an interactive Office session."""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path
from tempfile import TemporaryDirectory
from contextlib import contextmanager


@contextmanager
def word_source(path: Path, converter: Path | None = None, timeout: int = 120):
    if path.suffix.lower() != ".doc":
        yield path
        return
    configured = converter or os.getenv("RAG_WORD_CONVERTER_PATH")
    executable = str(configured) if configured else shutil.which("soffice")
    if not executable:
        for base in (os.getenv("PROGRAMFILES"), os.getenv("PROGRAMFILES(X86)")):
            candidate = Path(base or ".") / "LibreOffice/program/soffice.exe"
            if candidate.is_file():
                executable = str(candidate)
                break
    if not executable:
        raise ValueError("DOC conversion requires LibreOffice; set RAG_WORD_CONVERTER_PATH")
    with TemporaryDirectory(prefix="rag-word-") as directory:
        root = Path(directory)
        profile = root / "profile"
        user = profile / "user"
        user.mkdir(parents=True)
        # A private profile prevents joining the user's Office process or loading user macros.
        (user / "registrymodifications.xcu").write_text(
            '<?xml version="1.0" encoding="UTF-8"?><oor:items xmlns:oor="http://openoffice.org/2001/registry">'
            '<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop></item>'
            '<item oor:path="/org.openoffice.Office.Writer/Content/Update"><prop oor:name="Link" oor:op="fuse"><value>2</value></prop></item>'
            '<item oor:path="/org.openoffice.Office.Writer/Content/Update"><prop oor:name="Field" oor:op="fuse"><value>false</value></prop><prop oor:name="Chart" oor:op="fuse"><value>false</value></prop></item>'
            '</oor:items>', encoding="utf-8",
        )
        command = [executable, f"-env:UserInstallation={profile.as_uri()}", "--headless", "--nologo",
                   "--nodefault", "--norestore", "--convert-to", "docx:Office Open XML Text",
                   "--outdir", str(root), str(path.resolve())]
        try:
            process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                       creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
            try:
                out, err = process.communicate(timeout=timeout)
            except subprocess.TimeoutExpired:
                if os.name == "nt":
                    subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"], capture_output=True,
                                   creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
                else:
                    process.kill()
                process.communicate()
                raise ValueError(f"DOC conversion exceeded {timeout} seconds: {path.name}") from None
        except OSError as exc:
            raise ValueError(f"DOC converter could not start: {path.name}: {exc}") from exc
        converted = root / f"{path.stem}.docx"
        if process.returncode or not converted.is_file():
            detail = (err or out).decode("utf-8", errors="replace").strip()[-400:]
            raise ValueError(f"DOC conversion failed (invalid or encrypted document): {path.name}: {detail}")
        yield converted
