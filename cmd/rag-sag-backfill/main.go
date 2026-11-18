// Command rag-sag-backfill builds the SAG event-entity index for the
// documents of one namespace that were published before the extraction
// stage existed. It resolves the extraction model from the Console model
// profiles (kind=extraction), reads each document's active chunk points from
// Qdrant, extracts one event plus its entities per chunk, and writes the
// sag_events/sag_entities/sag_event_entities rows in MySQL.
//
// Startup mirrors cmd/rag-mcp-ingest: LoadStartupConfig → service settings →
// RagConfig → state database → runtime.New. The backfill needs
// RAG_BACKEND=qdrant and a configured knowledge base for the target
// namespace.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// Exit codes: 0 success, 1 runtime failure, 2 usage/configuration errors.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr *os.File) int {
	flags := flag.NewFlagSet("rag-sag-backfill", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "target knowledge-base namespace (default RAG_NAMESPACE)")
	dryRun := flags.Bool("dry-run", false, "list the documents that would be backfilled without extracting")
	verbose := flags.Bool("verbose", false, "print per-document progress")
	flags.Usage = func() {}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			printUsage(stdout)
			return exitOK
		}
		printUsage(stderr)
		return exitUsage
	}

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
		return configFailure("SAG 回填需要 RAG_BACKEND=qdrant")
	}
	if ragConfig.QdrantPath != "" {
		return configFailure("SAG 回填不支持 QDRANT_PATH；请设置 QDRANT_URL")
	}
	resolvedNamespace := strings.TrimSpace(*namespace)
	if resolvedNamespace == "" {
		resolvedNamespace = strings.TrimSpace(ragConfig.Namespace)
	}
	if resolvedNamespace == "" {
		return configFailure("RAG_NAMESPACE 必填（或传入 --namespace）")
	}
	if err := kbconfig.ValidateNamespace(resolvedNamespace); err != nil {
		return configFailure(err.Error())
	}

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *dryRun {
		documentIDs, err := stores.SagIndex.ListDocumentsWithoutEvents(ctx, resolvedNamespace)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		for _, documentID := range documentIDs {
			fmt.Fprintf(stdout, "PENDING %s\n", documentID)
		}
		fmt.Fprintf(stdout, "SUMMARY %d pending\n", len(documentIDs))
		return exitOK
	}

	service, err := runtime.New(ragConfig, stores, serviceSettings.DataRoot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer func() { _ = service.Close(context.Background()) }()

	processed, err := service.BackfillSagIndex(ctx, resolvedNamespace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if *verbose {
		fmt.Fprintf(stdout, "backfilled %d documents (namespace: %s)\n", processed, resolvedNamespace)
	}
	fmt.Fprintf(stdout, "SUMMARY %d backfilled\n", processed)
	return exitOK
}

func printUsage(out *os.File) {
	fmt.Fprint(out, `rag-sag-backfill：为已发布的文档补建 SAG 事件-实体索引

用法：
  rag-sag-backfill [--namespace <ns>] [--dry-run] [--verbose]

说明：
  读取 Qdrant 中每个文档的活跃 chunk，调用 kind=extraction 的模型
  （Console「模型配置」）抽取事件与实体，写入 MySQL 的
  sag_events / sag_entities / sag_event_entities 三张表。
  已有索引的文档跳过；无活跃点的文档标记处理但不产生行。
  抽取失败不阻塞其余文档（与入库管线的非致命语义一致）。
`)
}
