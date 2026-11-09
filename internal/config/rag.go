package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DenseVectorSize is the fixed BGE-M3 dense dimension.
const DenseVectorSize = 1024

// DefaultRerankerModel mirrors rag_mcp.rerankers.cross_encoder.DEFAULT_RERANKER_MODEL.
const DefaultRerankerModel = "BAAI/bge-reranker-base"

// BackendName is the supported retrieval backend selector.
type BackendName string

const (
	BackendMock   BackendName = "mock"
	BackendQdrant BackendName = "qdrant"
)

// RagConfig is the validated, environment-backed retrieval configuration kept
// outside the Agent-visible tool schema. It mirrors rag_mcp.config.RagConfig.
type RagConfig struct {
	Backend               BackendName
	QdrantURL             string
	QdrantPath            string
	Collection            string
	EmbeddingModel        string
	EmbeddingDevice       string
	ChunkTokenizerModel   string
	VectorSize            int
	PreloadModel          bool
	Namespace             string
	RerankerModel         string
	RerankerDevice        string
	RerankerBatchSize     int
	RerankerMaxCandidates int
	RerankerMaxLength     int
	RerankerMaxTextChars  int
	PreloadReranker       bool
	PDFParserPython       string
	PDFModelsPath         string
	PDFParseTimeout       int
	// VLMBaseURL is the OpenAI-compatible endpoint for formula recognition and
	// picture descriptions. Empty means the parser has no remote VLM and
	// document parsing fails when it needs one.
	VLMBaseURL         string
	VLMAPIKey          string
	VLMModel           string
	VLMTimeout         int
	VLMConcurrency     int
	WordConverterPath  string
	WordConvertTimeout int
}

// RagConfigFromEnv loads configuration without connecting to Qdrant or loading
// a model. Error messages stay identical to the Python implementation.
func RagConfigFromEnv() (RagConfig, error) {
	rawBackend := strings.ToLower(strings.TrimSpace(os.Getenv("RAG_BACKEND")))
	if rawBackend == "" {
		rawBackend = string(BackendMock)
	}
	if rawBackend != string(BackendMock) && rawBackend != string(BackendQdrant) {
		return RagConfig{}, fmt.Errorf("RAG_BACKEND must be mock or qdrant")
	}

	qdrantURL := optionalEnv("QDRANT_URL")
	rawPath := optionalEnv("QDRANT_PATH")
	if qdrantURL != "" && rawPath != "" {
		return RagConfig{}, fmt.Errorf("Set only one of QDRANT_URL and QDRANT_PATH")
	}
	qdrantPath := ""
	if rawPath != "" {
		qdrantPath = ExpandPath(rawPath)
	}
	if BackendName(rawBackend) == BackendQdrant && qdrantURL == "" && qdrantPath == "" {
		qdrantURL = "http://localhost:6333"
	}

	collection := strings.TrimSpace(os.Getenv("RAG_COLLECTION"))
	if collection == "" {
		collection = "mini_agent_knowledge_pdf_assets_v1"
	}

	rawSize := strings.TrimSpace(os.Getenv("RAG_DENSE_VECTOR_SIZE"))
	if rawSize == "" {
		rawSize = "1024"
	}
	vectorSize, err := parseIntSetting("RAG_DENSE_VECTOR_SIZE", rawSize)
	if err != nil {
		return RagConfig{}, err
	}
	if vectorSize != DenseVectorSize {
		return RagConfig{}, fmt.Errorf("BGE-M3 dense vectors must have %d dimensions", DenseVectorSize)
	}

	embeddingModel := strings.TrimSpace(os.Getenv("RAG_EMBEDDING_MODEL"))
	if embeddingModel == "" {
		embeddingModel = "BAAI/bge-m3"
	}
	rerankerModel := strings.TrimSpace(os.Getenv("RAG_RERANKER_MODEL"))
	if rerankerModel == "" {
		rerankerModel = DefaultRerankerModel
	}
	rerankerDevice := optionalEnv("RAG_RERANKER_DEVICE")
	if rerankerDevice == "" {
		rerankerDevice = optionalEnv("RAG_EMBEDDING_DEVICE")
	}
	chunkTokenizer := optionalEnv("RAG_CHUNK_TOKENIZER_MODEL")
	if chunkTokenizer == "" {
		chunkTokenizer = "BAAI/bge-m3"
	}

	preloadModel, err := booleanEnv("RAG_PRELOAD_MODEL", false)
	if err != nil {
		return RagConfig{}, err
	}
	preloadReranker, err := booleanEnv("RAG_PRELOAD_RERANKER", false)
	if err != nil {
		return RagConfig{}, err
	}
	batchSize, err := integerEnv("RAG_RERANKER_BATCH_SIZE", 4, 1, 64)
	if err != nil {
		return RagConfig{}, err
	}
	maxCandidates, err := integerEnv("RAG_RERANKER_MAX_CANDIDATES", 30, 1, 100)
	if err != nil {
		return RagConfig{}, err
	}
	maxLength, err := integerEnv("RAG_RERANKER_MAX_LENGTH", 512, 64, 8192)
	if err != nil {
		return RagConfig{}, err
	}
	maxTextChars, err := integerEnv("RAG_RERANKER_MAX_TEXT_CHARS", 4000, 256, 100_000)
	if err != nil {
		return RagConfig{}, err
	}
	pdfTimeout, err := integerEnv("RAG_PDF_PARSE_TIMEOUT", 900, 30, 7200)
	if err != nil {
		return RagConfig{}, err
	}
	wordTimeout, err := integerEnv("RAG_WORD_CONVERT_TIMEOUT", 120, 1, 7200)
	if err != nil {
		return RagConfig{}, err
	}
	vlmBaseURL, err := parseVLMBaseURL(os.Getenv("RAG_VLM_BASE_URL"))
	if err != nil {
		return RagConfig{}, err
	}
	vlmModel := strings.TrimSpace(os.Getenv("RAG_VLM_MODEL"))
	if vlmBaseURL != "" && vlmModel == "" {
		return RagConfig{}, fmt.Errorf("RAG_VLM_MODEL is required when RAG_VLM_BASE_URL is set")
	}
	vlmTimeout, err := integerEnv("RAG_VLM_TIMEOUT", 120, 1, 3600)
	if err != nil {
		return RagConfig{}, err
	}
	vlmConcurrency, err := integerEnv("RAG_VLM_CONCURRENCY", 4, 1, 64)
	if err != nil {
		return RagConfig{}, err
	}

	return RagConfig{
		Backend:               BackendName(rawBackend),
		QdrantURL:             qdrantURL,
		QdrantPath:            qdrantPath,
		Collection:            collection,
		EmbeddingModel:        embeddingModel,
		EmbeddingDevice:       optionalEnv("RAG_EMBEDDING_DEVICE"),
		ChunkTokenizerModel:   chunkTokenizer,
		VectorSize:            vectorSize,
		PreloadModel:          preloadModel,
		Namespace:             optionalEnv("RAG_NAMESPACE"),
		RerankerModel:         rerankerModel,
		RerankerDevice:        rerankerDevice,
		RerankerBatchSize:     batchSize,
		RerankerMaxCandidates: maxCandidates,
		RerankerMaxLength:     maxLength,
		RerankerMaxTextChars:  maxTextChars,
		PreloadReranker:       preloadReranker,
		PDFParserPython:       optionalPath("RAG_PDF_PARSER_PYTHON"),
		PDFModelsPath:         optionalPath("RAG_PDF_MODELS_PATH"),
		PDFParseTimeout:       pdfTimeout,
		VLMBaseURL:            vlmBaseURL,
		VLMAPIKey:             optionalEnv("RAG_VLM_API_KEY"),
		VLMModel:              vlmModel,
		VLMTimeout:            vlmTimeout,
		VLMConcurrency:        vlmConcurrency,
		WordConverterPath:     optionalPath("RAG_WORD_CONVERTER_PATH"),
		WordConvertTimeout:    wordTimeout,
	}, nil
}

// parseVLMBaseURL validates the remote VLM endpoint shape.
func parseVLMBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("RAG_VLM_BASE_URL must be an http(s) URL, for example https://llm.internal/v1")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("RAG_VLM_BASE_URL must not embed credentials; set RAG_VLM_API_KEY instead")
	}
	return strings.TrimRight(value, "/"), nil
}

// ExpandPath expands a leading "~" and normalizes the path.
func ExpandPath(value string) string {
	expanded := expandHome(value)
	if absolute, err := filepath.Abs(expanded); err == nil {
		return absolute
	}
	return expanded
}

// expandHome expands a leading "~" using the current user home directory.
func expandHome(value string) string {
	if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		home, err := os.UserHomeDir()
		if err == nil {
			if value == "~" {
				return home
			}
			return filepath.Join(home, value[2:])
		}
	}
	return value
}

func optionalPath(name string) string {
	value := optionalEnv(name)
	if value == "" {
		return ""
	}
	return ExpandPath(value)
}

func parseIntSetting(name, raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return value, nil
}
