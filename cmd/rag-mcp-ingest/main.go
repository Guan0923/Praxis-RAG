// Command rag-mcp-ingest imports local Markdown/TXT/PDF/DOCX/DOC documents into
// the configured Qdrant knowledge base. It is the Go port of
// The former Python ingest.py: walk a file or directory (sorted, recursive), index each
// supported document through the shared runtime upload pipeline, print one OK/
// SKIP/FAIL line per document, and finish with the Python SUMMARY line.
//
// Startup mirrors cmd/rag-service: LoadStartupConfig → service settings →
// RagConfig → state database → runtime.New. Ingestion requires RAG_BACKEND=qdrant
// and a configured knowledge base for the target namespace.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/document"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
	"github.com/Guan0923/xinghe_rag/internal/redisstate"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Exit codes: 0 success, 1 runtime/ingest failure (Python main returns 1 when
// any document failed), 2 usage/configuration errors (the CLI contract; note
// argparse itself already uses 2 for usage errors).
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// options are the resolved CLI arguments.
type options struct {
	path      string
	namespace string
	recursive bool
	dryRun    bool
	verbose   bool
}

func run(args []string, stdout, stderr *os.File) int {
	flags := flag.NewFlagSet("rag-mcp-ingest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "document file or directory (alternative to the positional path)")
	namespace := flags.String("namespace", "", "target knowledge-base namespace (default RAG_NAMESPACE)")
	recursive := flags.Bool("recursive", true, "walk directories recursively (Python ingest always recurses)")
	dryRun := flags.Bool("dry-run", false, "list the documents that would be imported without indexing")
	verbose := flags.Bool("verbose", false, "print configuration and per-document progress")
	// Usage is rendered explicitly so --help goes to stdout exactly once (the
	// flag package would otherwise print it to stderr and ErrHelp would add a
	// second copy).
	flags.Usage = func() {}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			printUsage(stdout)
			return exitOK
		}
		printUsage(stderr)
		return exitUsage
	}
	positional := flags.Args()
	if len(positional) > 1 {
		fmt.Fprintln(stderr, "rag-mcp-ingest: expected a single document path")
		printUsage(stderr)
		return exitUsage
	}
	resolved := options{
		namespace: strings.TrimSpace(*namespace),
		recursive: *recursive,
		dryRun:    *dryRun,
		verbose:   *verbose,
	}
	if len(positional) == 1 {
		resolved.path = positional[0]
	}
	if resolved.path != "" && *dir != "" {
		fmt.Fprintln(stderr, "rag-mcp-ingest: pass either a path or --dir, not both")
		printUsage(stderr)
		return exitUsage
	}
	if resolved.path == "" {
		resolved.path = *dir
	}
	if resolved.path == "" {
		fmt.Fprintln(stderr, "rag-mcp-ingest: the document path is required")
		printUsage(stderr)
		return exitUsage
	}

	// Config-stage failures are usage/configuration errors (exit 2).
	configFailure := func(message string) int {
		fmt.Fprintln(stderr, message)
		return exitUsage
	}
	if _, err := config.LoadStartupConfig(); err != nil {
		return configFailure(err.Error())
	}
	serviceSettings, err := config.ServiceSettingsFromEnv()
	if err != nil {
		return configFailure(err.Error())
	}
	ragConfig, err := config.RagConfigFromEnv()
	if err != nil {
		return configFailure(err.Error())
	}
	if ragConfig.Backend != config.BackendQdrant {
		return configFailure("Document ingestion requires RAG_BACKEND=qdrant")
	}
	if ragConfig.QdrantPath != "" {
		return configFailure("Document ingestion does not support QDRANT_PATH; set QDRANT_URL")
	}
	if resolved.namespace == "" {
		resolved.namespace = strings.TrimSpace(ragConfig.Namespace)
	}
	if resolved.namespace == "" {
		return configFailure("RAG_NAMESPACE is required (or pass --namespace)")
	}
	if err := kbconfig.ValidateNamespace(resolved.namespace); err != nil {
		return configFailure(err.Error())
	}

	documents, sourceRoot, err := discoverDocuments(resolved.path, resolved.recursive)
	if err != nil {
		return configFailure(err.Error())
	}
	if resolved.dryRun {
		for _, path := range documents {
			fmt.Fprintf(stdout, "DRY  %s\n", path)
		}
		printSummary(stdout, len(documents), 0, 0, 0)
		return exitOK
	}

	// Runtime-stage failures are exit 1.
	stateSettings, err := state.SettingsFromEnv(serviceSettings.DataRoot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	stores, err := state.New(stateSettings, stateSettings.AutoSchema)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer func() { _ = stores.Close() }()
	// Ingestion writes documents through the shared journal, so it must join the
	// deployment's document lease backend instead of holding a second, MySQL-only
	// lock that the other processes cannot see.
	lockSettings, err := state.DocumentLockSettingsFromEnv()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if lockSettings.RedisMode() {
		documentLock, lockErr := redisstate.NewDocumentLock(lockSettings)
		if lockErr != nil {
			fmt.Fprintln(stderr, lockErr)
			return exitError
		}
		defer func() { _ = documentLock.Close() }()
		if attachErr := stores.AttachDocumentLocker(documentLock); attachErr != nil {
			fmt.Fprintln(stderr, attachErr)
			return exitError
		}
	}
	service, err := runtime.New(ragConfig, stores, serviceSettings.DataRoot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer func() { _ = service.Close(context.Background()) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	settings, err := stores.KnowledgeBases.Get(ctx, resolved.namespace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if settings == nil {
		fmt.Fprintf(stderr, "%s（namespace: %s）\n", kbconfig.MessageKnowledgeBaseMissing, resolved.namespace)
		return exitUsage
	}
	if resolved.verbose {
		fmt.Fprintf(stdout, "Using namespace=%s collection=%s model=%s\n",
			resolved.namespace, settingText(settings, "collection"), settingText(settings, "embedding_model"))
	}

	importedFiles := 0
	importedChunks := 0
	skippedFiles := 0
	failedFiles := 0
	existing, listErr := service.ListDocuments(ctx, resolved.namespace)
	if listErr != nil {
		fmt.Fprintln(stderr, listErr)
		return exitError
	}
	known := make(map[string]bool, len(existing))
	for _, record := range existing {
		if name, ok := record["name"].(string); ok {
			known[name] = true
		}
	}
	for _, path := range documents {
		display := displayPath(path, sourceRoot)
		name, nameErr := runtime.SanitizeName(display)
		if nameErr != nil {
			fmt.Fprintf(stdout, "FAIL %s: %s\n", path, nameErr)
			failedFiles++
			continue
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			fmt.Fprintf(stdout, "FAIL %s: %s\n", path, safeLine(readErr.Error()))
			failedFiles++
			continue
		}
		if resolved.verbose {
			fmt.Fprintf(stdout, "INGEST %s (name=%s, bytes=%d)\n", path, name, len(content))
		}
		overwrite := known[name]
		result, writeErr := service.WriteDocument(ctx, resolved.namespace, name, content, overwrite, "")
		if writeErr != nil {
			message := safeLine(writeErr.Error())
			if strings.Contains(message, "没有可索引的文本") {
				fmt.Fprintf(stdout, "SKIP %s: empty document\n", path)
				continue
			}
			fmt.Fprintf(stdout, "FAIL %s: %s\n", path, message)
			failedFiles++
			continue
		}
		status, _ := result["status"].(string)
		documentRecord, _ := result["document"].(map[string]any)
		chunkCount, _ := documentRecord["chunk_count"].(int)
		switch status {
		case "added", "updated":
			known[name] = true
			importedFiles++
			importedChunks += chunkCount
			fmt.Fprintf(stdout, "OK   %s (%d chunks)\n", path, chunkCount)
		case "unchanged":
			// The pipeline deduplicates identical bytes for the same name instead
			// of re-embedding them (Python re-embeds unconditionally).
			skippedFiles++
			fmt.Fprintf(stdout, "SKIP %s: unchanged content\n", path)
		default:
			failedFiles++
			fmt.Fprintf(stdout, "FAIL %s: unexpected write status %q\n", path, status)
		}
	}
	if resolved.verbose && skippedFiles > 0 {
		fmt.Fprintf(stdout, "Skipped %d unchanged document(s).\n", skippedFiles)
	}
	printSummary(stdout, len(documents), importedFiles, importedChunks, failedFiles)
	if failedFiles > 0 {
		return exitError
	}
	return exitOK
}

// discoverDocuments ports chunking.discover_documents: a supported file is
// accepted directly, a directory yields its supported files in sorted order.
// sourceRoot is the directory that relative display names are derived from.
func discoverDocuments(rawPath string, recursive bool) ([]string, string, error) {
	expanded := config.ExpandPath(rawPath)
	info, err := os.Stat(expanded)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("Document path does not exist: %s", expanded)
		}
		return nil, "", fmt.Errorf("Document path is not readable: %s", expanded)
	}
	if !info.IsDir() {
		if !document.IsSupportedSuffix(filepath.Ext(expanded)) {
			return nil, "", fmt.Errorf("Unsupported document type: %s", suffixOrNone(expanded))
		}
		return []string{expanded}, filepath.Dir(expanded), nil
	}
	discovered := make([]string, 0)
	if recursive {
		walkErr := filepath.WalkDir(expanded, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if !document.IsSupportedSuffix(filepath.Ext(path)) {
				return nil
			}
			discovered = append(discovered, path)
			return nil
		})
		if walkErr != nil {
			return nil, "", fmt.Errorf("Cannot read %s: %v", expanded, walkErr)
		}
	} else {
		entries, readErr := os.ReadDir(expanded)
		if readErr != nil {
			return nil, "", fmt.Errorf("Cannot read %s: %v", expanded, readErr)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(expanded, entry.Name())
			if !document.IsSupportedSuffix(filepath.Ext(path)) {
				continue
			}
			discovered = append(discovered, path)
		}
	}
	return discovered, expanded, nil
}

// displayPath mirrors Python's source naming: documents inside a directory are
// identified by their relative POSIX path, a single file by its base name.
func displayPath(path, sourceRoot string) string {
	relative, err := filepath.Rel(sourceRoot, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return filepath.Base(path)
	}
	return filepath.ToSlash(relative)
}

func suffixOrNone(path string) string {
	suffix := strings.ToLower(filepath.Ext(path))
	if suffix == "" {
		return "<none>"
	}
	return suffix
}

func settingText(settings map[string]any, key string) string {
	if value, ok := settings[key].(string); ok && value != "" {
		return value
	}
	return "-"
}

func safeLine(message string) string {
	return strings.Join(strings.Fields(message), " ")
}

// printSummary renders the exact Python SUMMARY line.
func printSummary(stdout *os.File, discovered, imported, chunks, failed int) {
	fmt.Fprintf(stdout, "SUMMARY discovered=%d imported=%d chunks=%d failed=%d\n",
		discovered, imported, chunks, failed)
}

func printUsage(writer *os.File) {
	fmt.Fprint(writer, `Import Markdown/TXT/PDF/DOCX/DOC documents into Qdrant with BGE-M3 and BM25 sparse vectors.

Usage:
  rag-mcp-ingest [flags] <path>

Arguments:
  path                  Document file or directory to import

Flags:
  --dir <path>          Alternative to the positional path
  --namespace <name>    Target knowledge-base namespace (default: RAG_NAMESPACE)
  --recursive           Walk directories recursively (default true; Python ingest always recurses)
  --dry-run             List the discovered documents without indexing them
  --verbose             Print configuration and per-document progress
  -h, --help            Show this help and exit

Exit codes:
  0  every supported document was imported
  1  at least one document failed, or the runtime could not be prepared
  2  usage or configuration error (bad arguments, RAG_BACKEND, missing knowledge base)
`)
}
