package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DOCConverter converts legacy .doc files to .docx through an external
// converter. It mirrors the contextmanager in rag_mcp.word_converter: the
// caller receives the converted path plus a cleanup function.
type DOCConverter interface {
	Convert(ctx context.Context, source string) (converted string, cleanup func(), err error)
}

// sofficeDefaultTimeout matches word_converter's default timeout.
const sofficeDefaultTimeout = 120 * time.Second

// SofficeConverter converts .doc through a headless LibreOffice process using
// a private user profile (no interactive Office session, no user macros).
type SofficeConverter struct {
	// Executable is the LibreOffice binary; empty means auto-detect through
	// RAG_WORD_CONVERTER_PATH, PATH and the standard program directories.
	Executable string
	// Timeout bounds one conversion; zero means 120 seconds (or
	// RAG_WORD_CONVERT_TIMEOUT when constructed from the environment).
	Timeout time.Duration
}

// Convert implements DOCConverter.
func (c *SofficeConverter) Convert(ctx context.Context, source string) (string, func(), error) {
	if strings.ToLower(filepath.Ext(source)) != SuffixDOC {
		return source, nil, nil
	}
	executable := c.executable()
	if executable == "" {
		return "", nil, fmt.Errorf("DOC conversion requires LibreOffice; set RAG_WORD_CONVERTER_PATH")
	}
	root, err := os.MkdirTemp("", "rag-word-*")
	if err != nil {
		return "", nil, fmt.Errorf("DOC converter could not start: %s: %v", filepath.Base(source), err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	profile := filepath.Join(root, "profile")
	user := filepath.Join(profile, "user")
	if err := os.MkdirAll(user, 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("DOC converter could not start: %s: %v", filepath.Base(source), err)
	}
	// A private profile prevents joining the user's Office process or loading
	// user macros, mirroring word_converter.py.
	settings := `<?xml version="1.0" encoding="UTF-8"?><oor:items xmlns:oor="http://openoffice.org/2001/registry">` +
		`<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop></item>` +
		`<item oor:path="/org.openoffice.Office.Writer/Content/Update"><prop oor:name="Link" oor:op="fuse"><value>2</value></prop></item>` +
		`<item oor:path="/org.openoffice.Office.Writer/Content/Update"><prop oor:name="Field" oor:op="fuse"><value>false</value></prop><prop oor:name="Chart" oor:op="fuse"><value>false</value></prop></item>` +
		`</oor:items>`
	if err := os.WriteFile(filepath.Join(user, "registrymodifications.xcu"), []byte(settings), 0o644); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("DOC converter could not start: %s: %v", filepath.Base(source), err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = wordConvertTimeout()
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, executable,
		"-env:UserInstallation="+profileURI(profile),
		"--headless", "--nologo", "--nodefault", "--norestore",
		"--convert-to", "docx:Office Open XML Text",
		"--outdir", root, source,
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		cleanup()
		return "", nil, fmt.Errorf("DOC conversion exceeded %d seconds: %s", int(timeout/time.Second), filepath.Base(source))
	}
	if runErr != nil {
		var execError *exec.Error
		if errors.As(runErr, &execError) {
			cleanup()
			return "", nil, fmt.Errorf("DOC converter could not start: %s: %v", filepath.Base(source), runErr)
		}
	}
	converted := filepath.Join(root, strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))+".docx")
	if runErr != nil || !fileExists(converted) {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		detail = lastRunes(detail, 400)
		cleanup()
		return "", nil, fmt.Errorf("DOC conversion failed (invalid or encrypted document): %s: %s",
			filepath.Base(source), detail)
	}
	return converted, cleanup, nil
}

func (c *SofficeConverter) executable() string {
	if c != nil && c.Executable != "" {
		return c.Executable
	}
	if configured := os.Getenv("RAG_WORD_CONVERTER_PATH"); configured != "" {
		return configured
	}
	if path, err := exec.LookPath("soffice"); err == nil {
		return path
	}
	for _, base := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)")} {
		if base == "" {
			continue
		}
		candidate := filepath.Join(base, "LibreOffice", "program", "soffice.exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// profileURI renders a path the way pathlib.Path.as_uri() does on Windows
// (file:///C:/...), which LibreOffice accepts for -env:UserInstallation.
func profileURI(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	return (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(absolute)}).String()
}

// wordConvertTimeout reads RAG_WORD_CONVERT_TIMEOUT the way
// document_readers._docx_blocks does, defaulting to 120 seconds.
func wordConvertTimeout() time.Duration {
	raw := os.Getenv("RAG_WORD_CONVERT_TIMEOUT")
	if raw == "" {
		return sofficeDefaultTimeout
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return sofficeDefaultTimeout
	}
	if seconds <= 0 {
		return sofficeDefaultTimeout
	}
	return time.Duration(seconds) * time.Second
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// lastRunes bounds a diagnostic string to n runes from the end.
func lastRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[len(runes)-n:])
}

// docxDocument mirrors document_readers._docx_blocks: legacy conversion when
// needed, archive validation, parse, then the Python error wrapping semantics.
func (r *Reader) docxDocument(ctx context.Context, resolved, name, suffix string) ([]Block, string, error) {
	source := resolved
	if suffix == SuffixDOC {
		converter := r.DOC
		if converter == nil {
			converter = &SofficeConverter{}
		}
		converted, cleanup, err := converter.Convert(ctx, resolved)
		if err != nil {
			return nil, "", readErrf("Word 解析失败：%s；%v", name, err)
		}
		if cleanup != nil {
			defer cleanup()
		}
		source = converted
	}
	displayName := filepath.Base(source)
	if err := validateDOCX(source, displayName); err != nil {
		// Validation errors are already DocumentReadError sequivalents and
		// propagate unchanged, like document_readers._docx_blocks.
		return nil, "", err
	}
	blocks, err := parseDOCX(source)
	if err != nil {
		return nil, "", readErrf("Word 解析失败：%s；%v", name, err)
	}
	return blocks, joinDOCX(blocks), nil
}
