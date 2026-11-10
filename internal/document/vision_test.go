package document

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

// visionCall captures one decoded vision request for assertions.
type visionCall struct {
	model  string
	prompt string
	image  image.Image
	auth   string
}

func decodeVisionCall(t *testing.T, request *http.Request) visionCall {
	t.Helper()
	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		t.Fatalf("vision request is not valid JSON: %v", err)
	}
	call := visionCall{model: payload.Model, auth: request.Header.Get("Authorization")}
	for _, message := range payload.Messages {
		for _, part := range message.Content {
			switch part.Type {
			case "text":
				call.prompt = part.Text
			case "image_url":
				const prefix = "data:image/png;base64,"
				if !strings.HasPrefix(part.ImageURL.URL, prefix) {
					t.Fatalf("image url prefix = %q", part.ImageURL.URL)
				}
				raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(part.ImageURL.URL, prefix))
				if err != nil {
					t.Fatalf("image base64 = %v", err)
				}
				decoded, err := png.Decode(bytes.NewReader(raw))
				if err != nil {
					t.Fatalf("image is not PNG: %v", err)
				}
				call.image = decoded
			}
		}
	}
	return call
}

func writeVisionResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": content}}},
	})
}

func TestVisionDescribeMissingCaptionsSuccess(t *testing.T) {
	pkg, dir := prepareVisionStaging(t)
	if missing := MissingPictureCaptions(pkg.Manifest); len(missing) != 1 || missing[0] != "picture-1" {
		t.Fatalf("missing = %v", missing)
	}
	var call visionCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		call = decodeVisionCall(t, r)
		writeVisionResponse(w, "A red indicator is lit on the control panel")
	}))
	defer server.Close()
	worker := &WordVisionWorker{VLM: VLMSettings{
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-vlm",
		Timeout: 5 * time.Second,
	}}
	if err := worker.DescribeMissingCaptions(context.Background(), dir); err != nil {
		t.Fatalf("DescribeMissingCaptions() error = %v", err)
	}
	if call.auth != "Bearer test-key" || call.model != "test-vlm" {
		t.Fatalf("call = %+v", call)
	}
	if call.prompt == "" || !strings.Contains(call.prompt, "Describe the visible content") {
		t.Fatalf("prompt = %q", call.prompt)
	}
	if call.image == nil {
		t.Fatalf("image was not sent")
	}
	if bounds := call.image.Bounds(); bounds.Dx() > 384 || bounds.Dy() > 384 {
		t.Fatalf("image size = %v, want max edge <= 384", bounds)
	}
	if err := pkg.ReloadFromStaging(dir); err != nil {
		t.Fatalf("ReloadFromStaging() error = %v", err)
	}
	want := "A red indicator is lit on the control panel."
	if pkg.Manifest.Assets[0].GeneratedDescription != want {
		t.Fatalf("description = %q, want %q", pkg.Manifest.Assets[0].GeneratedDescription, want)
	}
	content, err := BuildWordContent("doc-v", pkg)
	if err != nil {
		t.Fatalf("BuildWordContent() error = %v", err)
	}
	payload := content.Assets[0].Payload("vision.docx")
	if payload["generated_description"] != want {
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
	calls := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeVisionResponse(w, "unused")
	}))
	defer server.Close()
	worker := &WordVisionWorker{VLM: VLMSettings{BaseURL: server.URL + "/v1", Model: "test-vlm"}}
	if err := worker.DescribeMissingCaptions(context.Background(), dir); err != nil {
		t.Fatalf("DescribeMissingCaptions() error = %v", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("vision endpoint was called for a captioned picture")
	}
}

func TestVisionRequiresRemoteVLM(t *testing.T) {
	_, dir := prepareVisionStaging(t)
	worker := &WordVisionWorker{}
	err := worker.DescribeMissingCaptions(context.Background(), dir)
	want := "Word image description failed: RAG_VLM_BASE_URL is required for uncaptioned Word images"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v", err)
	}
	missingModel := &WordVisionWorker{VLM: VLMSettings{BaseURL: "http://vlm.test/v1"}}
	err = missingModel.DescribeMissingCaptions(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "RAG_VLM_MODEL is required") {
		t.Fatalf("missing model error = %v", err)
	}
}

func TestVisionRejectsRefusal(t *testing.T) {
	_, dir := prepareVisionStaging(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeVisionResponse(w, "I'm sorry, but I cannot provide a description of this image.")
	}))
	defer server.Close()
	worker := &WordVisionWorker{VLM: VLMSettings{BaseURL: server.URL + "/v1", Model: "test-vlm"}}
	err := worker.DescribeMissingCaptions(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "Vision did not describe the image") {
		t.Fatalf("error = %v", err)
	}
}

func TestVisionRetriesTransientStatus(t *testing.T) {
	previousDelay := visionRequestRetryDelay
	visionRequestRetryDelay = 5 * time.Millisecond
	defer func() { visionRequestRetryDelay = previousDelay }()
	_, dir := prepareVisionStaging(t)
	attempts := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&attempts, 1)
		if attempt < 3 {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		writeVisionResponse(w, "A green pump panel.")
	}))
	defer server.Close()
	worker := &WordVisionWorker{VLM: VLMSettings{BaseURL: server.URL + "/v1", Model: "test-vlm"}}
	if err := worker.DescribeMissingCaptions(context.Background(), dir); err != nil {
		t.Fatalf("DescribeMissingCaptions() error = %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestVisionHTTPFailure(t *testing.T) {
	_, dir := prepareVisionStaging(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()
	worker := &WordVisionWorker{VLM: VLMSettings{BaseURL: server.URL + "/v1", Model: "test-vlm"}}
	err := worker.DescribeMissingCaptions(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "Remote VLM returned HTTP 400") {
		t.Fatalf("error = %v", err)
	}
}

func TestVisionTimeout(t *testing.T) {
	previousDelay := visionRequestRetryDelay
	previousMinimum := visionMinimumRequestTimeout
	visionRequestRetryDelay = time.Millisecond
	visionMinimumRequestTimeout = 20 * time.Millisecond
	defer func() {
		visionRequestRetryDelay = previousDelay
		visionMinimumRequestTimeout = previousMinimum
	}()
	_, dir := prepareVisionStaging(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		writeVisionResponse(w, "too late")
	}))
	defer server.Close()
	worker := &WordVisionWorker{VLM: VLMSettings{BaseURL: server.URL + "/v1", Model: "test-vlm", Timeout: 20 * time.Millisecond}}
	err := worker.DescribeMissingCaptions(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "Remote VLM call failed after 3 attempts") {
		t.Fatalf("error = %v", err)
	}
}

func TestVisionUndecodableImage(t *testing.T) {
	_, dir := prepareVisionStaging(t)
	manifest, err := ReadWordManifestFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("ReadWordManifestFile() error = %v", err)
	}
	if len(manifest.Assets) == 0 || manifest.Assets[0].ImageFile == nil {
		t.Fatalf("manifest has no picture asset")
	}
	imagePath := filepath.Join(dir, filepath.FromSlash(*manifest.Assets[0].ImageFile))
	if err := os.WriteFile(imagePath, []byte("not an image"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	worker := &WordVisionWorker{VLM: VLMSettings{BaseURL: "http://vlm.test/v1", Model: "test-vlm"}}
	err = worker.DescribeMissingCaptions(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "not a decodable image") {
		t.Fatalf("error = %v", err)
	}
}

func TestVisionMissingManifest(t *testing.T) {
	worker := &WordVisionWorker{VLM: VLMSettings{BaseURL: "http://vlm.test/v1", Model: "test-vlm"}}
	err := worker.DescribeMissingCaptions(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "Word image description failed") ||
		!strings.Contains(err.Error(), "manifest is missing") {
		t.Fatalf("error = %v", err)
	}
}

func TestVisionChatURLAndCleanDescription(t *testing.T) {
	cases := map[string]string{
		"http://vlm.test":                   "http://vlm.test/v1/chat/completions",
		"http://vlm.test/v1":                "http://vlm.test/v1/chat/completions",
		"http://vlm.test/v1/chat/completions": "http://vlm.test/v1/chat/completions",
		"http://vlm.test/v1/":               "http://vlm.test/v1/chat/completions",
	}
	for base, want := range cases {
		if got := vlmChatURL(base); got != want {
			t.Fatalf("vlmChatURL(%q) = %q, want %q", base, got, want)
		}
	}
	if got := cleanVisionDescription(" A pump. <end_of_utterance>"); got != "A pump." {
		t.Fatalf("cleanVisionDescription() = %q", got)
	}
	if got := cleanVisionDescription("A red lamp"); got != "A red lamp." {
		t.Fatalf("cleanVisionDescription() = %q", got)
	}
	if got := cleanVisionDescription("已经完成描述"); got != "已经完成描述." {
		t.Fatalf("cleanVisionDescription(CJK) = %q", got)
	}
}
