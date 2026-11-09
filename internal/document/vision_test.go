package document

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeVisionRunner mutates the manifest like word_vision_worker.py would.
type fakeVisionRunner struct {
	calls            int
	args             []string
	env              map[string]string
	stdout, stderr   []byte
	err              error
	blockUntilCancel bool
	describe         string
}

func (f *fakeVisionRunner) Run(ctx context.Context, name string, args []string, env map[string]string) ([]byte, []byte, error) {
	f.calls++
	f.args = args
	f.env = env
	if f.blockUntilCancel {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	if f.err != nil {
		return f.stdout, f.stderr, f.err
	}
	if f.describe != "" {
		manifestPath := argumentValue(args, "--manifest")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, []byte(err.Error()), err
		}
		var manifest map[string]any
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return nil, nil, err
		}
		assets, _ := manifest["assets"].([]any)
		for _, item := range assets {
			asset, _ := item.(map[string]any)
			if asset["kind"] != "picture" || asset["caption"] != "" {
				continue
			}
			asset["generated_description"] = f.describe
		}
		updated, err := json.Marshal(manifest)
		if err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(manifestPath, updated, 0o644); err != nil {
			return nil, nil, err
		}
	}
	return f.stdout, f.stderr, nil
}

func prepareVisionStaging(t *testing.T) (*WordPackage, string) {
	t.Helper()
	body := pictureParagraph("rId1", "") + paragraphXML("Body text")
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	dir := t.TempDir()
	if err := pkg.WriteStaging(dir); err != nil {
		t.Fatalf("WriteStaging() error = %v", err)
	}
	return pkg, dir
}

func TestValidateVisionDescription(t *testing.T) {
	refusals := []string{
		"I'm sorry, but I cannot provide a description of this image.",
		"I cannot view the image.",
		"Unable to describe the picture.",
		"As an AI, I do not have access.",
		"抱歉，无法描述这张图片。",
	}
	for _, refusal := range refusals {
		if err := ValidateVisionDescription(refusal); err == nil ||
			!strings.Contains(err.Error(), "Vision did not describe the image") {
			t.Fatalf("refusal %q error = %v", refusal, err)
		}
	}
	if err := ValidateVisionDescription("A red indicator is lit on the control panel."); err != nil {
		t.Fatalf("valid description error = %v", err)
	}
	if err := ValidateVisionDescription("   "); err == nil {
		t.Fatalf("empty description accepted")
	}
}

func TestVisionDescribeMissingCaptionsSuccess(t *testing.T) {
	pkg, dir := prepareVisionStaging(t)
	if missing := MissingPictureCaptions(pkg.Manifest); len(missing) != 1 || missing[0] != "picture-1" {
		t.Fatalf("missing = %v", missing)
	}
	runner := &fakeVisionRunner{describe: "A red indicator is lit on the control panel."}
	worker := &WordVisionWorker{
		Executable: "test-python",
		PythonPath: "/package/root",
		ModelsPath: "/models",
		VLM:        testVLM,
		Timeout:    5 * time.Second,
		Runner:     runner,
	}
	if err := worker.DescribeMissingCaptions(context.Background(), dir); err != nil {
		t.Fatalf("DescribeMissingCaptions() error = %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d", runner.calls)
	}
	if argumentValue(runner.args, "-m") != "rag_mcp.word_vision_worker" {
		t.Fatalf("args = %v", runner.args)
	}
	if argumentValue(runner.args, "--manifest") != filepath.Join(dir, "manifest.json") {
		t.Fatalf("manifest arg = %q", argumentValue(runner.args, "--manifest"))
	}
	if argumentValue(runner.args, "--models") != "/models" {
		t.Fatalf("models arg = %q", argumentValue(runner.args, "--models"))
	}
	if runner.env["PYTHONUTF8"] != "1" || !strings.HasPrefix(runner.env["PYTHONPATH"], "/package/root") {
		t.Fatalf("env = %v", runner.env)
	}
	if runner.env["RAG_VLM_BASE_URL"] != "http://vlm.test/v1" || runner.env["RAG_VLM_MODEL"] != "test-vlm" ||
		runner.env["RAG_VLM_API_KEY"] != "test-key" {
		t.Fatalf("vlm env = %v", runner.env)
	}
	if err := pkg.ReloadFromStaging(dir); err != nil {
		t.Fatalf("ReloadFromStaging() error = %v", err)
	}
	if pkg.Manifest.Assets[0].GeneratedDescription != "A red indicator is lit on the control panel." {
		t.Fatalf("description = %q", pkg.Manifest.Assets[0].GeneratedDescription)
	}
	content, err := BuildWordContent("doc-v", pkg)
	if err != nil {
		t.Fatalf("BuildWordContent() error = %v", err)
	}
	payload := content.Assets[0].Payload("vision.docx")
	if payload["generated_description"] != "A red indicator is lit on the control panel." {
		t.Fatalf("payload generated_description = %v", payload["generated_description"])
	}
}

func TestVisionSkipsWorkerWhenNothingMissing(t *testing.T) {
	body := pictureParagraph("rId1", "") + paragraphXML("Figure 1: captioned")
	path := wordPicturePackage(t, body)
	pkg, err := ParseWordPackage(path)
	if err != nil {
		t.Fatalf("ParseWordPackage() error = %v", err)
	}
	dir := t.TempDir()
	if err := pkg.WriteStaging(dir); err != nil {
		t.Fatalf("WriteStaging() error = %v", err)
	}
	runner := &fakeVisionRunner{}
	worker := &WordVisionWorker{ModelsPath: "/models", VLM: testVLM, Runner: runner}
	if err := worker.DescribeMissingCaptions(context.Background(), dir); err != nil {
		t.Fatalf("DescribeMissingCaptions() error = %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("runner calls = %d", runner.calls)
	}
}

func TestVisionRequiresRemoteVLM(t *testing.T) {
	_, dir := prepareVisionStaging(t)
	runner := &fakeVisionRunner{describe: "unused"}
	worker := &WordVisionWorker{Runner: runner}
	err := worker.DescribeMissingCaptions(context.Background(), dir)
	want := "Word image description failed: RAG_VLM_BASE_URL is required for uncaptioned Word images"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("runner was called without a remote VLM")
	}
}

// testVLM is the remote endpoint every worker test configures.
var testVLM = VLMSettings{
	BaseURL:     "http://vlm.test/v1",
	APIKey:      "test-key",
	Model:       "test-vlm",
	Timeout:     30 * time.Second,
	Concurrency: 2,
}

func TestVisionRejectsRefusal(t *testing.T) {
	_, dir := prepareVisionStaging(t)
	runner := &fakeVisionRunner{describe: "I'm sorry, but I cannot provide a description of this image."}
	worker := &WordVisionWorker{ModelsPath: "/models", VLM: testVLM, Runner: runner}
	err := worker.DescribeMissingCaptions(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "Vision did not describe the image") {
		t.Fatalf("error = %v", err)
	}
}

func TestVisionProcessFailureDetail(t *testing.T) {
	_, dir := prepareVisionStaging(t)
	runner := &fakeVisionRunner{
		err:    errors.New("exit status 1"),
		stderr: []byte("Traceback (most recent call last):\n  File x\nValueError: models are missing\n"),
	}
	worker := &WordVisionWorker{ModelsPath: "/models", VLM: testVLM, Runner: runner}
	err := worker.DescribeMissingCaptions(context.Background(), dir)
	if err == nil || err.Error() != "Word image description failed: ValueError: models are missing" {
		t.Fatalf("error = %v", err)
	}
	runner = &fakeVisionRunner{err: errors.New("exit status 1"), stdout: []byte("line one\nlast stdout line")}
	worker = &WordVisionWorker{ModelsPath: "/models", VLM: testVLM, Runner: runner}
	err = worker.DescribeMissingCaptions(context.Background(), dir)
	if err == nil || err.Error() != "Word image description failed: last stdout line" {
		t.Fatalf("stdout detail error = %v", err)
	}
}

func TestVisionTimeout(t *testing.T) {
	_, dir := prepareVisionStaging(t)
	runner := &fakeVisionRunner{blockUntilCancel: true}
	worker := &WordVisionWorker{ModelsPath: "/models", VLM: testVLM, Runner: runner, Timeout: 20 * time.Millisecond}
	err := worker.DescribeMissingCaptions(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v", err)
	}
}

func TestVisionMissingManifest(t *testing.T) {
	worker := &WordVisionWorker{ModelsPath: "/models", VLM: testVLM, Runner: &fakeVisionRunner{}}
	err := worker.DescribeMissingCaptions(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "Word image description failed") ||
		!strings.Contains(err.Error(), "manifest is missing") {
		t.Fatalf("error = %v", err)
	}
}
