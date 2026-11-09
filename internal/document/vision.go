package document

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// WriteFile writes the manifest as JSON (the manifest.json artifact).
func (m WordManifest) WriteFile(path string) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("Word manifest cannot be serialized: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("Word manifest is not writable: %v", err)
	}
	return nil
}

// ReadWordManifestFile reads a manifest JSON artifact.
func ReadWordManifestFile(path string) (WordManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return WordManifest{}, fmt.Errorf("Word manifest is missing: %v", err)
	}
	var manifest WordManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return WordManifest{}, fmt.Errorf("Word manifest is invalid: %v", err)
	}
	return manifest, nil
}

// WriteStaging writes parser-relative images and manifest.json into a staging
// directory, the layout the external word_vision_worker expects (manifest.json
// plus image files referenced by each asset's image_file). It does not qualify
// ids or paths with the document id yet: that happens in BuildWordContent.
func (p *WordPackage) WriteStaging(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("Word staging directory is not writable: %v", err)
	}
	for _, asset := range p.Manifest.Assets {
		if asset.Kind != "picture" || asset.ImageFile == nil {
			continue
		}
		data, ok := p.Images[asset.AssetID]
		if !ok {
			return fmt.Errorf("Word picture data is missing: %s", asset.AssetID)
		}
		target := filepath.Join(dir, filepath.FromSlash(*asset.ImageFile))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("Word image artifact is not writable: %v", err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("Word image artifact is not writable: %s", *asset.ImageFile)
		}
	}
	return p.Manifest.WriteFile(filepath.Join(dir, "manifest.json"))
}

// ReloadFromStaging replaces the in-memory manifest with the manifest.json in a
// staging directory (used after the external vision worker rewrites it).
func (p *WordPackage) ReloadFromStaging(dir string) error {
	manifest, err := ReadWordManifestFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	p.Manifest = manifest
	return nil
}

// visionRefusal mirrors the refusal/generic-response filter in
// word_vision_worker.validate_description.
var visionRefusal = regexp.MustCompile(`(?i)i\s+(?:cannot|can't|do not|don't)|unable to|cannot (?:view|see|access|analy[sz]e)|i(?:'m| am) sorry|as an ai|抱歉|无法(?:查看|识别|分析|描述)|不能(?:查看|识别|分析)`)

// ValidateVisionDescription rejects empty, refusal or generic vision output,
// mirroring word_vision_worker.validate_description.
func ValidateVisionDescription(text string) error {
	if strings.TrimSpace(text) == "" || visionRefusal.MatchString(text) {
		return fmt.Errorf("Vision did not describe the image; refusing to index a refusal or generic response")
	}
	return nil
}

// MissingPictureCaptions returns the manifest ids of pictures without a
// document caption; they need the external vision worker (or stay as
// placeholders when no vision model is configured).
func MissingPictureCaptions(manifest WordManifest) []string {
	missing := make([]string, 0, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		if asset.Kind == "picture" && asset.Caption == "" {
			missing = append(missing, asset.AssetID)
		}
	}
	return missing
}

// WordVisionRunner runs one external vision worker command and captures its
// output so the caller can report a bounded diagnostic line.
type WordVisionRunner interface {
	Run(ctx context.Context, name string, args []string, env map[string]string) (stdout, stderr []byte, err error)
}

// WordVisionWorker is the Go client of the external word_vision_worker protocol
// (src/rag_mcp/word_vision_worker.py): it describes uncaptioned Word pictures
// with the existing Granite Vision model. The Go service never embeds Python;
// when no vision model is configured the worker is not invoked and pictures
// stay captioned by placeholders/[Image] text only, or the caller chooses to
// reject the document. Tests inject a fake runner.
type WordVisionWorker struct {
	// Executable is the Python interpreter. Empty means "python".
	Executable string
	// Module is the worker module path. Empty means "rag_mcp.word_vision_worker".
	Module string
	// PythonPath is prepended to PYTHONPATH so the worker package imports.
	PythonPath string
	// ModelsPath is passed as --models; it is required when pictures are
	// uncaptioned because the model artifacts live there.
	ModelsPath string
	// Timeout bounds one worker run. Zero means 900 seconds.
	Timeout time.Duration
	// Runner overrides process execution for tests.
	Runner WordVisionRunner
}

// DescribeMissingCaptions runs the vision worker for the uncaptioned pictures of
// the manifest stored in stagingDir/manifest.json, then reloads that manifest
// (with generated_description filled in) and validates every produced
// description.
//
// The caller workflow is: ParseWordPackage -> WriteStaging -> (if
// MissingPictureCaptions) DescribeMissingCaptions -> ReloadFromStaging ->
// BuildWordContent.
func (w *WordVisionWorker) DescribeMissingCaptions(ctx context.Context, stagingDir string) error {
	manifestPath := filepath.Join(stagingDir, "manifest.json")
	manifest, err := ReadWordManifestFile(manifestPath)
	if err != nil {
		return fmt.Errorf("Word image description failed: %v", err)
	}
	missing := MissingPictureCaptions(manifest)
	if len(missing) == 0 {
		return nil
	}
	if w.ModelsPath == "" {
		// The Python worker raises ValueError when --models is absent; surface
		// the same safe failure without spawning a process.
		return fmt.Errorf("Word image description failed: ValueError: RAG_PDF_MODELS_PATH is required for uncaptioned Word images")
	}
	executable := w.Executable
	if executable == "" {
		executable = "python"
	}
	module := w.Module
	if module == "" {
		module = "rag_mcp.word_vision_worker"
	}
	args := []string{"-m", module, "--manifest", manifestPath, "--models", w.ModelsPath}
	env := map[string]string{"PYTHONUTF8": "1"}
	if w.PythonPath != "" {
		existing := os.Getenv("PYTHONPATH")
		if existing != "" {
			env["PYTHONPATH"] = w.PythonPath + string(os.PathListSeparator) + existing
		} else {
			env["PYTHONPATH"] = w.PythonPath
		}
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 900 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runner := w.Runner
	if runner == nil {
		runner = execWordVisionRunner{}
	}
	stdout, stderr, runErr := runner.Run(runCtx, executable, args, env)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("Word image description timed out after %d seconds", int(timeout/time.Second))
	}
	if runErr != nil {
		return fmt.Errorf("Word image description failed: %s", visionDiagnostic(stdout, stderr, runErr))
	}
	reloaded, err := ReadWordManifestFile(manifestPath)
	if err != nil {
		return fmt.Errorf("Word image description failed: %v", err)
	}
	byID := make(map[string]*WordManifestAsset, len(reloaded.Assets))
	for _, asset := range reloaded.Assets {
		byID[asset.AssetID] = asset
	}
	for _, assetID := range missing {
		asset, ok := byID[assetID]
		if !ok {
			return fmt.Errorf("Word image description failed: asset disappeared from the manifest: %s", assetID)
		}
		if err := ValidateVisionDescription(asset.GeneratedDescription); err != nil {
			return err
		}
	}
	return nil
}

// visionDiagnostic mirrors word_assets.parse_word's stderr detail extraction:
// the last output line, bounded to 400 characters.
func visionDiagnostic(stdout, stderr []byte, runErr error) string {
	raw := stderr
	if len(raw) == 0 {
		raw = stdout
	}
	text := strings.TrimSpace(string(raw))
	detail := ""
	if text != "" {
		lines := strings.Split(text, "\n")
		detail = strings.TrimSpace(lines[len(lines)-1])
	}
	if detail == "" {
		detail = runErr.Error()
	}
	runes := []rune(detail)
	if len(runes) > 400 {
		return string(runes[len(runes)-400:])
	}
	return detail
}

// execWordVisionRunner is the production WordVisionRunner.
type execWordVisionRunner struct{}

func (execWordVisionRunner) Run(ctx context.Context, name string, args []string, env map[string]string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = mergeEnvironment(env)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
