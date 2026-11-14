// Package logging owns the process-wide structured logger and the log-only
// helpers the service layers share: request/task correlation, error
// classification, and redaction of operator-visible diagnostic text.
//
// The package deliberately imports nothing from the project so every layer
// (state, runtime, workers, HTTP) can depend on it without an import cycle. The
// classification helpers therefore resolve against small optional interfaces
// (ErrorCategory, HTTPStatus, FailureStage) that the domain error types
// implement.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Environment variable names understood by Setup.
const (
	// LevelEnvVar selects the minimum level: debug, info, warn or error.
	LevelEnvVar = "RAG_LOG_LEVEL"
	// FormatEnvVar selects the handler encoding: text (default) or json.
	FormatEnvVar = "RAG_LOG_FORMAT"
	// DebugTraceEnvVar is the legacy debug switch. It still enables debug level
	// when LevelEnvVar is unset, and remains the model-client trace switch.
	DebugTraceEnvVar = "RAG_DEBUG_TRACE"
)

// Format is the slog handler encoding selected by RAG_LOG_FORMAT.
type Format string

// Supported formats.
const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// Options is the resolved logging configuration.
type Options struct {
	Level  slog.Level
	Format Format
}

// FromEnv reads RAG_LOG_LEVEL, RAG_LOG_FORMAT and the legacy RAG_DEBUG_TRACE.
// An unset level defaults to info, or debug when RAG_DEBUG_TRACE is truthy.
// Invalid values are configuration errors: the entry points fail fast instead
// of silently logging at the wrong level.
func FromEnv() (Options, error) {
	return optionsFromGetenv(os.Getenv)
}

func optionsFromGetenv(getenv func(string) string) (Options, error) {
	options := Options{Level: slog.LevelInfo, Format: FormatText}
	rawLevel := strings.ToLower(strings.TrimSpace(getenv(LevelEnvVar)))
	switch rawLevel {
	case "":
		if truthy(getenv(DebugTraceEnvVar)) {
			options.Level = slog.LevelDebug
		}
	case "debug":
		options.Level = slog.LevelDebug
	case "info":
		options.Level = slog.LevelInfo
	case "warn", "warning":
		options.Level = slog.LevelWarn
	case "error":
		options.Level = slog.LevelError
	default:
		return Options{}, fmt.Errorf("%s must be one of debug, info, warn, error (got %q)",
			LevelEnvVar, rawLevel)
	}
	rawFormat := strings.ToLower(strings.TrimSpace(getenv(FormatEnvVar)))
	switch rawFormat {
	case "", string(FormatText):
		options.Format = FormatText
	case string(FormatJSON):
		options.Format = FormatJSON
	default:
		return Options{}, fmt.Errorf("%s must be one of text, json (got %q)",
			FormatEnvVar, rawFormat)
	}
	return options, nil
}

// New builds the process logger writing to stderr.
func New(options Options) *slog.Logger {
	return NewWithWriter(os.Stderr, options)
}

// NewWithWriter builds a logger with an explicit sink (tests use a buffer). It
// keeps the plain slog sink only: the Console capture ring and the optional
// JSON Lines file are installed by Setup, the process entry point.
func NewWithWriter(writer io.Writer, options Options) *slog.Logger {
	return slog.New(sinkHandler(writer, options))
}

// sinkHandler builds the configured text/JSON handler for one writer.
func sinkHandler(writer io.Writer, options Options) slog.Handler {
	handlerOptions := &slog.HandlerOptions{Level: options.Level}
	if options.Format == FormatJSON {
		return slog.NewJSONHandler(writer, handlerOptions)
	}
	return slog.NewTextHandler(writer, handlerOptions)
}

// Setup resolves the configuration from the environment, installs the logger
// as slog.Default (so packages that do not receive an explicit logger still
// honour RAG_LOG_LEVEL/RAG_LOG_FORMAT), and returns it.
//
// Setup also owns the Console monitoring sources: it always installs the
// process-wide in-memory ring (RAG_LOG_BUFFER_SIZE) and, when RAG_LOG_FILE is
// set, appends the same records as JSON Lines to that file with source
// information. An unreadable log file is a configuration error so the process
// fails fast instead of silently losing the monitoring view.
func Setup() (*slog.Logger, error) {
	return SetupWithWriter(os.Stderr)
}

// SetupWithWriter is Setup with an explicit sink.
func SetupWithWriter(writer io.Writer) (*slog.Logger, error) {
	options, err := FromEnv()
	if err != nil {
		return nil, err
	}
	bufferSize, err := bufferSizeFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	filePath, fileHandler, err := openLogFile(options)
	if err != nil {
		return nil, err
	}
	handlers := make([]slog.Handler, 0, 2)
	handlers = append(handlers, sinkHandler(writer, options))
	if fileHandler != nil {
		handlers = append(handlers, fileHandler)
	}
	buffer := NewBuffer(bufferSize)
	logger := slog.New(&captureHandler{
		buffer:   buffer,
		minLevel: options.Level,
		next:     newTeeHandler(handlers),
	})
	publishProcessState(buffer, filePath)
	slog.SetDefault(logger)
	return logger, nil
}

// truthy reports the conventional environment-variable boolean spellings.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
