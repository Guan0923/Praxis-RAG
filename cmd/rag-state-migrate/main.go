// Command rag-state-migrate reports (default) or applies the shared state schema.
// Port of the CLI parts of rag_mcp/state/migrate.py (schema creation is idempotent
// and additive; data import lives in rag-service-migrate).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/migrate"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

func main() { os.Exit(run()) }

func run() int {
	flags := flag.NewFlagSet("rag-state-migrate", flag.ContinueOnError)
	apply := flags.Bool("apply", false, "create missing state tables/migrations; default reports readiness only")
	dataRoot := flags.String("data-root", "", "data root (default RAG_SERVICE_DATA_ROOT or ~/.praxis-rag/data)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	root := *dataRoot
	if root == "" {
		service, err := config.ServiceSettingsFromEnv()
		if err != nil {
			fmt.Fprintf(os.Stderr, "rag-state-migrate: %v\n", err)
			return 2
		}
		root = service.DataRoot
	}
	settings, err := state.SettingsFromEnv(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-state-migrate: %v\n", err)
		return 2
	}
	ctx := context.Background()
	var status migrate.StateStatus
	if *apply {
		status, err = migrate.EnsureStateSchema(ctx, settings)
	} else {
		status, err = migrate.StateSchemaStatus(ctx, settings)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-state-migrate: %v\n", err)
		return 1
	}
	mode := "report"
	if *apply {
		mode = "apply"
	}
	fmt.Printf("mode=%s backend=%s database=%v host=%v tables=%d schema=%s\n",
		mode, status.Backend, status.Database, status.Host, status.Tables,
		map[bool]string{true: "ready", false: "incomplete"}[status.SchemaGood])
	if len(status.Missing) > 0 {
		for _, table := range status.Missing {
			fmt.Printf("MISSING %s\n", table)
		}
	}
	if !status.SchemaGood {
		if *apply {
			return 1
		}
		return 1
	}
	return 0
}
