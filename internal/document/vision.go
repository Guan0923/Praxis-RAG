package document

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/image/draw"

	_ "image/gif"
	_ "image/jpeg"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
)

// Vision description tuning mirrors the former Python worker: a fixed prompt,
// a 256-token answer budget, three attempts for transient endpoint failures and
// a 384-pixel cap on the uploaded image edge.
const (
	visionDescriptionPrompt    = "Describe the visible content for document retrieval in 2 to 4 concise sentences. Preserve visible names, labels and relationships. For a table, summarize its subject only; never invent cell values. End with a complete sentence."
	visionMaxImageEdge         = 384
	visionDescriptionMaxTokens = 256
	visionRequestAttempts      = 3
	visionDefaultRequestTimeout = 120 * time.Second
)

// overlay variables so tests can shorten the minimum timeout and retry delay.
var (
	visionMinimumRequestTimeout = 30 * time.Second
	visionRequestRetryDelay     = 500 * time.Millisecond
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
// directory. It does not qualify ids or paths with the document id yet: that
// happens in BuildWordContent.
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
// staging directory (used after the vision describer rewrites it).
func (p *WordPackage) ReloadFromStaging(dir string) error {
	manifest, err := ReadWordManifestFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	p.Manifest = manifest
	return nil
}

// visionRefusal filters refusal/generic vision output before it is indexed.
var visionRefusal = regexp.MustCompile(`(?i)i\s+(?:cannot|can't|do not|don't)|unable to|cannot (?:view|see|access|analy[sz]e)|i(?:'m| am) sorry|as an ai|抱歉|无法(?:查看|识别|分析|描述)|不能(?:查看|识别|分析)`)

// ValidateVisionDescription rejects empty, refusal or generic vision output.
func ValidateVisionDescription(text string) error {
	if strings.TrimSpace(text) == "" || visionRefusal.MatchString(text) {
		return fmt.Errorf("Vision did not describe the image; refusing to index a refusal or generic response")
	}
	return nil
}

// MissingPictureCaptions returns the manifest ids of pictures without a
// document caption; they need the remote vision model (or stay as placeholders
// when no model is configured). Pictures without image bytes (for example an
// external HTML image the Docling service intentionally did not fetch) are not
// listed: the vision model has nothing to describe.
func MissingPictureCaptions(manifest WordManifest) []string {
	missing := make([]string, 0, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		if asset.Kind == "picture" && asset.Caption == "" && asset.ImageFile != nil {
			missing = append(missing, asset.AssetID)
		}
	}
	return missing
}

// WordVisionWorker describes uncaptioned Word pictures with the remote
// OpenAI-compatible vision endpoint. It runs in-process: no Python worker or
// subprocess is involved. When no endpoint is configured the caller keeps the
// pictures as placeholders instead of invoking the worker.
type WordVisionWorker struct {
	// VLM configures the remote vision-language endpoint. Required when the
	// document has uncaptioned pictures.
	VLM VLMSettings
	// Timeout bounds one VLM request. Zero means VLM.Timeout, else 120 seconds
	// (never less than 30 seconds).
	Timeout time.Duration
	// Client overrides the HTTP client for tests.
	Client *http.Client
}

// DescribeMissingCaptions describes every uncaptioned picture of the manifest
// stored in stagingDir/manifest.json, rewrites that manifest with
// generated_description filled in, and validates every produced description.
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
	if !w.VLM.Configured() {
		return fmt.Errorf("Word image description failed: RAG_VLM_BASE_URL is required for uncaptioned Word images")
	}
	if strings.TrimSpace(w.VLM.Model) == "" {
		return fmt.Errorf("Word image description failed: RAG_VLM_MODEL is required for uncaptioned Word images")
	}
	byID := make(map[string]*WordManifestAsset, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		byID[asset.AssetID] = asset
	}
	client := w.httpClient()
	for _, assetID := range missing {
		asset, ok := byID[assetID]
		if !ok || asset.ImageFile == nil {
			return fmt.Errorf("Word image description failed: picture asset has no image file: %s", assetID)
		}
		encoded, err := visionPNG(filepath.Join(stagingDir, filepath.FromSlash(*asset.ImageFile)))
		if err != nil {
			return fmt.Errorf("Word image description failed: %v", err)
		}
		description, err := w.describeImage(ctx, client, encoded)
		if err != nil {
			return fmt.Errorf("Word image description failed: %v", err)
		}
		asset.GeneratedDescription = cleanVisionDescription(description)
	}
	if err := manifest.WriteFile(manifestPath); err != nil {
		return fmt.Errorf("Word image description failed: %v", err)
	}
	reloaded, err := ReadWordManifestFile(manifestPath)
	if err != nil {
		return fmt.Errorf("Word image description failed: %v", err)
	}
	persisted := make(map[string]*WordManifestAsset, len(reloaded.Assets))
	for _, asset := range reloaded.Assets {
		persisted[asset.AssetID] = asset
	}
	for _, assetID := range missing {
		asset, ok := persisted[assetID]
		if !ok {
			return fmt.Errorf("Word image description failed: asset disappeared from the manifest: %s", assetID)
		}
		if err := ValidateVisionDescription(asset.GeneratedDescription); err != nil {
			return err
		}
	}
	return nil
}

// httpClient returns the configured client or a bounded default.
func (w *WordVisionWorker) httpClient() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return &http.Client{Timeout: w.requestTimeout()}
}

// requestTimeout resolves the per-request deadline.
func (w *WordVisionWorker) requestTimeout() time.Duration {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = w.VLM.Timeout
	}
	if timeout <= 0 {
		timeout = visionDefaultRequestTimeout
	}
	if timeout < visionMinimumRequestTimeout {
		timeout = visionMinimumRequestTimeout
	}
	return timeout
}

// describeImage performs one chat completion with the image and the fixed
// description prompt, retrying transient failures.
func (w *WordVisionWorker) describeImage(ctx context.Context, client *http.Client, pngBytes []byte) (string, error) {
	payload := map[string]any{
		"model":       w.VLM.Model,
		"temperature": 0.0,
		"max_tokens":  visionDescriptionMaxTokens,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "image_url", "image_url": map[string]string{
					"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes),
				}},
				{"type": "text", "text": visionDescriptionPrompt},
			},
		}},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("remote VLM request could not be encoded: %v", err)
	}
	endpoint := vlmChatURL(w.VLM.BaseURL)
	lastError := ""
	for attempt := 0; attempt < visionRequestAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * visionRequestRetryDelay):
			}
		}
		text, retryable, err := w.requestDescription(ctx, client, endpoint, encoded)
		if err == nil {
			return text, nil
		}
		if !retryable {
			return "", err
		}
		lastError = err.Error()
	}
	return "", fmt.Errorf("Remote VLM call failed after %d attempts: %s", visionRequestAttempts, lastError)
}

// requestDescription performs a single chat completion request.
func (w *WordVisionWorker) requestDescription(ctx context.Context, client *http.Client, endpoint string, payload []byte) (string, bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, w.requestTimeout())
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", false, fmt.Errorf("Remote VLM request could not be created: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if w.VLM.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+w.VLM.APIKey)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", true, fmt.Errorf("request failed: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	switch response.StatusCode {
	case http.StatusOK:
		text, err := vlmResponseText(body)
		if err != nil {
			return "", false, err
		}
		if strings.TrimSpace(text) == "" {
			return "", false, fmt.Errorf("Remote VLM returned an empty description")
		}
		return text, false, nil
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "", true, fmt.Errorf("HTTP %d: %s", response.StatusCode, truncateText(string(body), 200))
	default:
		return "", false, fmt.Errorf("Remote VLM returned HTTP %d: %s", response.StatusCode, truncateText(string(body), 200))
	}
}

// vlmChatURL resolves the chat completions endpoint from the configured base
// URL, accepting either the API root, a /v1 root or the full endpoint.
func vlmChatURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch {
	case strings.HasSuffix(baseURL, "/chat/completions"):
		return baseURL
	case strings.HasSuffix(baseURL, "/v1"):
		return baseURL + "/chat/completions"
	default:
		return baseURL + "/v1/chat/completions"
	}
}

// vlmResponseText extracts choices[0].message.content, accepting both a plain
// string and the content-part list some OpenAI-compatible servers return.
func vlmResponseText(body []byte) (string, error) {
	var decoded struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("Remote VLM returned invalid JSON")
	}
	if len(decoded.Choices) == 0 {
		return "", fmt.Errorf("Remote VLM response has no choices[0].message.content")
	}
	content := decoded.Choices[0].Message.Content
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return "", fmt.Errorf("Remote VLM response has no choices[0].message.content")
	}
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(part.Text)
	}
	return builder.String(), nil
}

// visionPNG decodes a Word picture, downscales it to the upload edge cap and
// re-encodes it as PNG.
func visionPNG(path string) ([]byte, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Vision input is missing: %s", filepath.Base(path))
	}
	defer handle.Close()
	source, _, err := image.Decode(handle)
	if err != nil {
		return nil, fmt.Errorf("Vision input is not a decodable image: %s", filepath.Base(path))
	}
	canvas := image.NewRGBA(source.Bounds())
	draw.Draw(canvas, canvas.Bounds(), source, source.Bounds().Min, draw.Src)
	scaled := scaleVisionImage(canvas)
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, scaled); err != nil {
		return nil, fmt.Errorf("Vision input could not be encoded: %s", filepath.Base(path))
	}
	return buffer.Bytes(), nil
}

// scaleVisionImage shrinks images larger than the edge cap, preserving aspect.
func scaleVisionImage(source image.Image) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= visionMaxImageEdge && height <= visionMaxImageEdge {
		return source
	}
	scale := float64(visionMaxImageEdge) / float64(width)
	if height > width {
		scale = float64(visionMaxImageEdge) / float64(height)
	}
	targetWidth := int(float64(width)*scale + 0.5)
	targetHeight := int(float64(height)*scale + 0.5)
	if targetWidth < 1 {
		targetWidth = 1
	}
	if targetHeight < 1 {
		targetHeight = 1
	}
	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	draw.CatmullRom.Scale(target, target.Bounds(), source, bounds, draw.Over, nil)
	return target
}

// cleanVisionDescription drops stray end-of-utterance markers and terminates
// the description with punctuation.
func cleanVisionDescription(text string) string {
	value := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(text, "<end_of_utterance>", ""), "<end_of_utteranc", ""))
	if value == "" {
		return value
	}
	last, _ := utf8.DecodeLastRuneInString(value)
	if !strings.ContainsRune(".!?。！？", last) {
		value += "."
	}
	return value
}

// truncateText bounds diagnostic text to a rune count.
func truncateText(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit])
}
