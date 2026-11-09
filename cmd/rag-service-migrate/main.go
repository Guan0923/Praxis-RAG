// Command rag-service-migrate previews (default) or applies the legacy
// Mini-Agent knowledge-source migration. Port of rag_mcp/migrate.py.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/migrate"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

type userIDsFlag []string

func (f *userIDsFlag) String() string { return fmt.Sprint([]string(*f)) }
func (f *userIDsFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() { os.Exit(run()) }

func run() int {
	flags := flag.NewFlagSet("rag-service-migrate", flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	legacyRoot := flags.String("legacy-root", filepath.Join(home, ".mini_agent"), "legacy Mini-Agent data root")
	apply := flags.Bool("apply", false, "perform copy + reindex; default is a read-only preview")
	var users userIDsFlag
	flags.Var(&users, "user-id", "restrict migration to one user; repeatable")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	selected := make(map[string]struct{}, len(users))
	for _, user := range users {
		selected[user] = struct{}{}
	}
	root, err := filepath.Abs(*legacyRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: invalid --legacy-root: %v\n", err)
		return 2
	}
	sources, err := migrate.DiscoverLegacySources(root, selected)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: %v\n", err)
		return 1
	}
	total := 0
	for _, source := range sources {
		total += len(source.Files)
	}
	mode := "preview"
	if *apply {
		mode = "apply"
	}
	fmt.Printf("legacy users=%d files=%d mode=%s\n", len(sources), total, mode)
	for _, source := range sources {
		fmt.Printf("%s: %d file(s) -> %s\n", source.UserID, len(source.Files), migrate.NamespaceForLegacyUser(source.UserID))
	}
	if !*apply || len(sources) == 0 {
		return 0
	}
	plan, err := migrate.BuildPlan(root, sources)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: %v\n", err)
		return 1
	}
	service, err := config.ServiceSettingsFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: %v\n", err)
		return 2
	}
	settings, err := state.SettingsFromEnv(service.DataRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: %v\n", err)
		return 2
	}
	stores, err := state.New(settings, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: %v\n", err)
		return 1
	}
	defer func() { _ = stores.Close() }()
	result := migrate.Apply(context.Background(), stores, plan, migrate.ApplyOptions{})
	fmt.Printf("imported=%d failed=%d\n", result.Imported, result.Failed)
	warnings := append([]string(nil), result.Warnings...)
	sort.Strings(warnings)
	for index, warning := range warnings {
		if index >= 20 {
			break
		}
		fmt.Printf("WARN %s\n", warning)
	}
	if result.Failed > 0 {
		return 1
	}
	return 0
}
