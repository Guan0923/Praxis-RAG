// Command rag-service-migrate previews (default) or applies the legacy
// Mini-Agent knowledge-source migration. Port of rag_mcp/migrate.py.
//
// --retire-outbox is the explicit, irreversible retirement step for the four
// transactional outbox tables replaced by Debezium table-level capture
// (docs/cdc-migration.md §9). It only runs with --yes and reports each table.
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

// retireOutboxTables drops the four retired transactional outbox tables after
// an explicit --yes confirmation. Without --yes it connects read-only, lists
// the tables that still exist and returns 2 without changing anything. The DDL
// itself lives in migrate.RetireOutbox so the CLI and tests share one contract.
func retireOutboxTables(confirmed bool) int {
	service, err := config.ServiceSettingsFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: 无法读取服务配置：%v\n", err)
		return 2
	}
	settings, err := state.SettingsFromEnv(service.DataRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: 无法读取状态库配置：%v\n", err)
		return 2
	}
	stores, err := state.New(settings, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: 无法连接状态库：%v\n", err)
		return 1
	}
	defer func() { _ = stores.Close() }()
	ctx := context.Background()

	present, err := migrate.OutboxTablesPresent(ctx, stores)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: 检查旧 outbox 表失败：%v\n", err)
		return 1
	}
	if !confirmed {
		fmt.Fprintln(os.Stderr, "rag-service-migrate: --retire-outbox 需要 --yes 确认；未执行任何 DROP。")
		if len(present) == 0 {
			fmt.Println("当前数据库中四张旧 outbox 表均不存在（可重复执行，幂等）。")
		} else {
			fmt.Printf("仍存在的旧 outbox 表（%d 张）：\n", len(present))
			for _, table := range present {
				fmt.Printf("  %-24s 存在\n", table)
			}
		}
		fmt.Println("确认 CDC 切换完成且无需回滚后，运行：rag-service-migrate --retire-outbox --yes")
		return 2
	}

	retirement, err := migrate.RetireOutbox(ctx, stores)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-service-migrate: 旧 outbox 表下线失败：%v\n", err)
		return 1
	}
	dropped := make(map[string]struct{}, len(retirement.Dropped))
	for _, table := range retirement.Dropped {
		dropped[table] = struct{}{}
	}
	fmt.Println("旧 outbox 表下线结果：")
	for _, table := range migrate.RetiredOutboxTables {
		if _, ok := dropped[table]; ok {
			fmt.Printf("  %-24s 已删除（本次 DROP TABLE IF EXISTS）\n", table)
		} else {
			fmt.Printf("  %-24s 不存在（无需删除）\n", table)
		}
	}
	fmt.Printf("汇总：删除 %d 张，原本不存在 %d 张；只删除不重建，其他表未改动。\n",
		len(retirement.Dropped), len(retirement.Missing))
	return 0
}

func run() int {
	flags := flag.NewFlagSet("rag-service-migrate", flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	legacyRoot := flags.String("legacy-root", filepath.Join(home, ".mini_agent"), "legacy Mini-Agent data root")
	apply := flags.Bool("apply", false, "perform copy + reindex; default is a read-only preview")
	retireOutbox := flags.Bool("retire-outbox", false,
		"drop the retired transactional outbox tables (requires --yes; never runs by default)")
	yes := flags.Bool("yes", false, "confirm --retire-outbox: irreversible DROP TABLE for four retired tables")
	var users userIDsFlag
	flags.Var(&users, "user-id", "restrict migration to one user; repeatable")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *retireOutbox {
		if *apply {
			fmt.Fprintln(os.Stderr, "rag-service-migrate: --retire-outbox 不能与 --apply 同时使用")
			return 2
		}
		return retireOutboxTables(*yes)
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
