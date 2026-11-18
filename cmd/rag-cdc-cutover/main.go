// Command rag-cdc-cutover runs the operator-controlled cutover from the retired
// transactional outboxes to Kafka Connect Debezium table-level capture. It is the CLI half of
// docs/cdc-migration.md §8 (存量切换方案): inventory the unfinished work, record
// the starting binlog position, bump schedule_version for every unfinished task,
// mark the read-model cache dirty, and finally verify the baseline.
//
// The CLI is deliberately explicit:
//   - every mutating mode has its own flag;
//   - --record-position needs the File/Position printed by SHOW MASTER STATUS;
//   - --backfill/--verify need an explicit --baseline;
//   - nothing runs without a mode.
//
// Logs contain only counts, statuses and key names: never task arguments,
// document content, query text or credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// Exit codes follow the repository convention used by the workers and CLIs:
// 0 = success, 1 = runtime/verification failure, 2 = usage/config error.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// Cutover phases recorded in cdc_cutover_state.phase. They are operator-visible
// markers only; the authoritative state is the MySQL tables themselves.
const (
	phasePositionRecorded = "position_recorded"
	phaseBackfilled       = "backfilled"
	phaseCacheBaseline    = "cache_baseline"
)

// requeueBatchDefault bounds one RequeueActive transaction. The loop keeps
// calling until no unfinished row is below the baseline.
const requeueBatchDefault = 200

func main() { os.Exit(run(os.Args[1:])) }

// options holds the parsed flags. Exactly one mode is selected unless --all is
// used.
type options struct {
	inventory         bool
	recordPosition    bool
	backfill          bool
	markCacheBaseline bool
	verify            bool
	all               bool

	file     string
	pos      int64
	baseline int64
	batch    int
}

func run(args []string) int {
	flags := flag.NewFlagSet("rag-cdc-cutover", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	inventory := flags.Bool("inventory", false,
		"只读盘点未完成的文档任务、缓存 key 与各 key revision，并打印 SHOW MASTER STATUS 指引")
	recordPosition := flags.Bool("record-position", false,
		"把 SHOW MASTER STATUS 的 File/Position 写入 cdc_cutover_state（需要 --file/--pos）")
	backfill := flags.Bool("backfill", false,
		"为未完成任务递增 schedule_version 到基线之上并重排（需要 --baseline）")
	markCacheBaseline := flags.Bool("mark-cache-baseline", false,
		"把所有读模型 key 标脏，由缓存消费者重建（CacheSynchronizer.MarkAllDirty）")
	verify := flags.Bool("verify", false,
		"断言活动任务 schedule_version >= baseline+1 并输出核对清单（需要 --baseline）")
	all := flags.Bool("all", false,
		"按顺序执行 inventory → record-position → backfill → mark-cache-baseline → verify")
	file := flags.String("file", "", "SHOW MASTER STATUS 的 File（--record-position/--all）")
	pos := flags.Int64("pos", 0, "SHOW MASTER STATUS 的 Position（--record-position/--all）")
	baseline := flags.Int64("baseline", 0, "schedule_version 基线（--backfill/--verify/--all）")
	batch := flags.Int("batch", requeueBatchDefault, "RequeueActive 单批行数（默认 200）")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}

	opts := options{
		inventory:         *inventory,
		recordPosition:    *recordPosition,
		backfill:          *backfill,
		markCacheBaseline: *markCacheBaseline,
		verify:            *verify,
		all:               *all,
		file:              *file,
		pos:               *pos,
		baseline:          *baseline,
		batch:             *batch,
	}
	if code := validateOptions(opts); code != exitOK {
		return code
	}

	ctx := context.Background()
	stores, code := openStores()
	if code != exitOK {
		return code
	}
	defer func() { _ = stores.Close() }()

	switch {
	case opts.inventory:
		return runInventory(ctx, stores)
	case opts.recordPosition:
		return runRecordPosition(ctx, stores, opts.file, opts.pos)
	case opts.backfill:
		return runBackfill(ctx, stores, opts.baseline, opts.batch)
	case opts.markCacheBaseline:
		return runMarkCacheBaseline(ctx, stores)
	case opts.verify:
		return runVerify(ctx, stores, opts.baseline)
	default:
		return runAll(ctx, stores, opts)
	}
}

// validateOptions rejects ambiguous or incomplete invocations before any
// database connection is opened. It returns 2 for usage errors.
func validateOptions(opts options) int {
	selected := 0
	for _, enabled := range []bool{opts.inventory, opts.recordPosition, opts.backfill,
		opts.markCacheBaseline, opts.verify} {
		if enabled {
			selected++
		}
	}
	if opts.all {
		if selected > 0 {
			return usageError("--all 不能与其他模式同时使用")
		}
	} else if selected != 1 {
		return usageError("必须且只能选择一个模式：--inventory/--record-position/--backfill/" +
			"--mark-cache-baseline/--verify/--all")
	}
	if (opts.recordPosition || opts.all) && (opts.file == "" || opts.pos <= 0) {
		return usageError("--record-position/--all 需要 --file=<File> 与 --pos=<Position>（来自 SHOW MASTER STATUS）")
	}
	if (opts.backfill || opts.verify || opts.all) && opts.baseline <= 0 {
		return usageError("--backfill/--verify/--all 需要 --baseline=<正整数>")
	}
	if opts.batch <= 0 {
		return usageError("--batch 必须为正整数")
	}
	return exitOK
}

// usageError prints a Chinese usage diagnostic and returns exit code 2.
func usageError(message string) int {
	fmt.Fprintf(os.Stderr, "rag-cdc-cutover: %s\n", message)
	fmt.Fprintln(os.Stderr, "运行 `rag-cdc-cutover --help` 查看全部参数。")
	return exitUsage
}

// openStores connects with the same RAG_STATE_*/RAG_MYSQL_* configuration as
// rag-service-migrate. Reply 2 for configuration errors and 1 for runtime
// connection failures.
func openStores() (*state.Stores, int) {
	service, err := config.ServiceSettingsFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 无法读取服务配置：%v\n", err)
		return nil, exitUsage
	}
	settings, err := state.SettingsFromEnv(service.DataRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 无法读取状态库配置：%v\n", err)
		return nil, exitUsage
	}
	stores, err := state.New(settings, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 无法连接状态库：%v\n", err)
		return nil, exitFailure
	}
	return stores, exitOK
}

// runInventory is the read-only pre-cutover report. It also prints the exact
// SHOW MASTER STATUS instructions the operator must follow inside the write
// freeze window; the CLI never guesses the binlog position itself.
func runInventory(ctx context.Context, stores *state.Stores) int {
	inventory, err := stores.CutoverInventory(ctx, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 盘点失败：%v\n", err)
		return exitFailure
	}
	recorded, err := stores.CutoverStateAll(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 读取 cdc_cutover_state 失败：%v\n", err)
		return exitFailure
	}

	fmt.Println("== CDC 切换盘点（只读，不修改任何业务/任务数据）==")
	printRecordedState(recorded, []string{
		state.CutoverKeyPhase,
		state.CutoverKeyCDCStartFile,
		state.CutoverKeyCDCStartPos,
		state.CutoverKeyCutoverAt,
		state.CutoverKeyScheduleBaseline,
	})
	printTaskInventory("document_jobs", inventory.DocumentJobs,
		state.DocumentJobQueued, state.DocumentJobProcessing)
	fmt.Printf("cache_revisions：key=%d dirty=%d（dirty 表示 refreshed_revision < revision，等待缓存消费者收敛）\n",
		inventory.CacheKeys, inventory.CacheDirtyKeys)
	if code := printCacheRevisions(ctx, stores); code != exitOK {
		return code
	}
	if inventory.DocumentJobs.Active == 0 {
		fmt.Println("未完成文档任务：0（--backfill 仍建议执行以记录基线，返回 0 次重排属正常）")
	}
	fmt.Println()
	fmt.Println("SHOW MASTER STATUS 指引（必须在停写窗口内完成）：")
	fmt.Println("  1. 用具备 REPLICATION CLIENT 权限的账号连接 MySQL 主库。")
	fmt.Println("  2. 执行 `SHOW MASTER STATUS;`（MySQL 8.0.22+ 也可用 `SHOW BINARY LOG STATUS;`）。")
	fmt.Println("  3. 记录 File 与 Position；启用了 GTID 时把 Executed_Gtid_Set 作为补充证据保存。")
	fmt.Println("  4. 执行 `rag-cdc-cutover --record-position --file=<File> --pos=<Position>`。")
	fmt.Println("  说明：采集位点只用于切换窗口核对，应用不读取 binlog 位点。")
	fmt.Println("  Kafka Connect 的 source offset 保存在 praxis_connect_offsets，本工具不读取也不迁移；")
	fmt.Println("  --backfill 只按 document_jobs 补投未完成通知，不能代替 Connect 的初始快照。")
	return exitOK
}

// printRecordedState prints the known cdc_cutover_state keys in a stable order.
func printRecordedState(recorded map[string]string, keys []string) {
	fmt.Println("cdc_cutover_state：")
	for _, key := range keys {
		value, ok := recorded[key]
		if !ok || value == "" {
			value = "未记录"
		}
		fmt.Printf("  %-18s %s\n", key, value)
	}
}

// printTaskInventory renders one CutoverTaskInventory.
func printTaskInventory(label string, inventory state.CutoverTaskInventory,
	queuedStatus, processingStatus string) {
	fmt.Printf("%s：未完成=%d（%s=%d %s=%d），最老未完成=%s，最小 schedule_version=%d\n",
		label, inventory.Active,
		queuedStatus, inventory.ByStatus[queuedStatus],
		processingStatus, inventory.ByStatus[processingStatus],
		formatSeconds(inventory.OldestActiveSeconds), inventory.MinActiveScheduleVersion)
}

// formatSeconds renders an age without exposing raw float noise. Zero means
// "no unfinished task" rather than an instant age.
func formatSeconds(seconds float64) string {
	if seconds <= 0 {
		return "无"
	}
	return fmt.Sprintf("%.1f 秒", seconds)
}

// inventoryKeyLimit bounds the per-key cache detail printed by --inventory.
// The full set stays queryable with plain SQL; the CLI avoids dumping an
// unbounded table into a terminal or log collector.
const inventoryKeyLimit = 200

// printCacheRevisions renders the §8.2 "各 key 当前 revision" list: kind,
// scope, revision, refreshed revision and whether the key is dirty. last_error
// text is deliberately not printed, only whether it is set.
func printCacheRevisions(ctx context.Context, stores *state.Stores) int {
	keys, err := stores.CutoverCacheRevisions(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: cache_revisions 明细读取失败：%v\n", err)
		return exitFailure
	}
	if len(keys) == 0 {
		fmt.Println("cache_revisions 明细：无记录（新库或尚未产生读模型 key）")
		return exitOK
	}
	shown := keys
	omitted := 0
	if len(shown) > inventoryKeyLimit {
		shown = shown[:inventoryKeyLimit]
		omitted = len(keys) - inventoryKeyLimit
	}
	fmt.Printf("cache_revisions 明细（共 %d 个 key；dirty = refreshed_revision < revision 或 last_error 非空）：\n",
		len(keys))
	fmt.Printf("  %-14s %-40s %10s %10s %-6s %-19s\n",
		"kind", "scope", "revision", "refreshed", "dirty", "updated_at")
	for _, key := range shown {
		dirty := key.RefreshedRevision < key.Revision || key.LastError != nil
		updated := "未知"
		if key.UpdatedAt > 0 {
			updated = time.Unix(int64(key.UpdatedAt), 0).UTC().Format("2006-01-02 15:04:05")
		}
		scope := key.Scope
		if scope == "" {
			scope = `""`
		}
		fmt.Printf("  %-14s %-40s %10d %10d %-6s %-19s\n",
			key.Kind, truncateScope(scope, 40), key.Revision, key.RefreshedRevision,
			dirtyLabel(dirty), updated)
	}
	if omitted > 0 {
		fmt.Printf("  ……还有 %d 个 key 未显示（可用 MySQL 直接查询 cache_revisions 查看全部）\n", omitted)
	}
	return exitOK
}

// truncateScope keeps long namespaces legible without splitting a rune.
func truncateScope(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

// dirtyLabel renders the dirty flag without inventing a count.
func dirtyLabel(dirty bool) string {
	if dirty {
		return "yes"
	}
	return "no"
}

// runRecordPosition stores the SHOW MASTER STATUS File/Position and opens the
// cutover window marker. Re-running overwrites the recorded position, which is
// intentional: operators may need to restart the window.
func runRecordPosition(ctx context.Context, stores *state.Stores, file string, pos int64) int {
	file = trimSpace(file)
	if file == "" || pos <= 0 {
		return usageError("--file 不能为空且 --pos 必须为正整数")
	}
	if previous, ok, err := stores.CutoverStateGet(ctx, state.CutoverKeyCDCStartFile); err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 读取旧位点失败：%v\n", err)
		return exitFailure
	} else if ok && previous != "" && previous != file {
		fmt.Printf("提示：覆盖已记录的起始文件 %q → %q\n", previous, file)
	}
	cutoverAt := time.Now().UTC().Format(time.RFC3339)
	values := []struct {
		key   string
		value string
	}{
		{state.CutoverKeyCDCStartFile, file},
		{state.CutoverKeyCDCStartPos, strconv.FormatInt(pos, 10)},
		{state.CutoverKeyCutoverAt, cutoverAt},
		{state.CutoverKeyPhase, phasePositionRecorded},
	}
	for _, item := range values {
		if err := stores.CutoverStateSet(ctx, item.key, item.value); err != nil {
			fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 写入 %s 失败：%v\n", item.key, err)
			return exitFailure
		}
	}
	fmt.Printf("已记录切换位点：file=%s pos=%d at=%s（phase=%s）\n", file, pos, cutoverAt, phasePositionRecorded)
	return exitOK
}

// runBackfill re-dispatches every unfinished task above the baseline by
// incrementing schedule_version inside MySQL. RequeueActive is idempotent, so
// the loop stops when both tables return zero affected rows; repeating the same
// command later is safe.
func runBackfill(ctx context.Context, stores *state.Stores, baseline int64, batch int) int {
	fmt.Printf("回填开始：baseline=%d batch=%d（只重排未完成任务，不修改任务参数）\n", baseline, batch)
	if err := stores.CutoverStateSet(ctx, state.CutoverKeyScheduleBaseline, strconv.FormatInt(baseline, 10)); err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 写入 schedule_baseline 失败：%v\n", err)
		return exitFailure
	}
	var documents int
	for round := 1; ; round++ {
		documentCount, err := stores.DocumentJobs.RequeueActive(ctx, batch, baseline)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rag-cdc-cutover: document_jobs 回填失败：%v\n", err)
			return exitFailure
		}
		documents += documentCount
		fmt.Printf("  第 %d 批：document_jobs=%d\n", round, documentCount)
		if documentCount == 0 {
			break
		}
		if round >= 10000 {
			fmt.Fprintln(os.Stderr, "rag-cdc-cutover: 回填批次超过上限，停止（请检查是否有持续写入）")
			return exitFailure
		}
	}
	if err := stores.CutoverStateSet(ctx, state.CutoverKeyPhase, phaseBackfilled); err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 写入 phase 失败：%v\n", err)
		return exitFailure
	}
	fmt.Printf("回填完成：document_jobs=%d（可重复执行，第二次返回 0）\n", documents)
	return exitOK
}

// runMarkCacheBaseline marks every known read-model key dirty so the cache
// consumer rebuilds it from MySQL. It never writes Redis directly.
func runMarkCacheBaseline(ctx context.Context, stores *state.Stores) int {
	if stores.CacheSync == nil {
		fmt.Fprintln(os.Stderr, "rag-cdc-cutover: 当前状态库未提供 CacheSynchronizer")
		return exitFailure
	}
	count, err := stores.CacheSync.MarkAllDirty(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 缓存基线标记失败：%v\n", err)
		return exitFailure
	}
	if err := stores.CutoverStateSet(ctx, state.CutoverKeyPhase, phaseCacheBaseline); err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 写入 phase 失败：%v\n", err)
		return exitFailure
	}
	fmt.Printf("缓存基线已标记：%d 个读模型 key 变脏（等待 rag-cache-worker/RefreshDirty 重建，不改 Redis 内容）\n", count)
	return exitOK
}

// runVerify asserts the cutover invariant and prints a checklist. It fails (1)
// only when an active task is still below baseline+1; an unfinalized cache
// watermark is a warning because the consumer converges asynchronously.
func runVerify(ctx context.Context, stores *state.Stores, baseline int64) int {
	inventory, err := stores.CutoverInventory(ctx, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 核对盘点失败：%v\n", err)
		return exitFailure
	}
	failures := 0
	fmt.Println("== CDC 切换核对清单 ==")

	recordedBaseline := ""
	if value, ok, err := stores.CutoverStateGet(ctx, state.CutoverKeyScheduleBaseline); err != nil {
		fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 读取 schedule_baseline 失败：%v\n", err)
		return exitFailure
	} else if ok {
		recordedBaseline = value
	}
	if recordedBaseline == "" {
		fmt.Printf("[提示] cdc_cutover_state 未记录 schedule_baseline；以 --baseline=%d 核对\n", baseline)
	} else if recordedBaseline != strconv.FormatInt(baseline, 10) {
		fmt.Printf("[失败] cdc_cutover_state.schedule_baseline=%s 与 --baseline=%d 不一致\n",
			recordedBaseline, baseline)
		failures++
	} else {
		fmt.Printf("[通过] cdc_cutover_state.schedule_baseline=%s\n", recordedBaseline)
	}

	failures += verifyTaskInventory("document_jobs", inventory.DocumentJobs, baseline)

	switch {
	case inventory.CacheDirtyKeys == 0:
		fmt.Printf("[通过] 缓存水位：key=%d，脏 key=0\n", inventory.CacheKeys)
	default:
		fmt.Printf("[警告] 缓存水位：key=%d，脏 key=%d（等待 rag-cache-worker/RefreshDirty 收敛，不阻塞任务核对）\n",
			inventory.CacheKeys, inventory.CacheDirtyKeys)
	}
	if file, ok, err := stores.CutoverStateGet(ctx, state.CutoverKeyCDCStartFile); err == nil && ok && file != "" {
		fmt.Printf("[通过] 已记录切换位点：%s\n", file)
	} else {
		fmt.Println("[提示] 尚未记录切换位点（--record-position），无法核对 binlog 起点")
	}

	if failures > 0 {
		fmt.Printf("核对结果：失败 %d 项（活动任务仍低于 baseline+1）\n", failures)
		return exitFailure
	}
	fmt.Println("核对结果：通过（所有活动任务 schedule_version >= baseline+1）")
	return exitOK
}

// verifyTaskInventory checks one task table and prints PASS/FAIL. A table with
// no unfinished rows passes trivially.
func verifyTaskInventory(label string, inventory state.CutoverTaskInventory, baseline int64) int {
	if inventory.Active == 0 {
		fmt.Printf("[通过] %s：无未完成任务\n", label)
		return 0
	}
	if inventory.MinActiveScheduleVersion >= baseline+1 {
		fmt.Printf("[通过] %s：未完成=%d，最小 schedule_version=%d >= %d\n",
			label, inventory.Active, inventory.MinActiveScheduleVersion, baseline+1)
		return 0
	}
	fmt.Printf("[失败] %s：未完成=%d，最小 schedule_version=%d < %d（请重跑 --backfill --baseline=%d）\n",
		label, inventory.Active, inventory.MinActiveScheduleVersion, baseline+1, baseline)
	return 1
}

// runAll executes the documented §8 sequence. `record-position` must be run
// inside the same write freeze window as `backfill`; aborting on the first
// failure keeps the sequence safe to resume with the individual mode flags.
func runAll(ctx context.Context, stores *state.Stores, opts options) int {
	steps := []struct {
		title string
		run   func() int
	}{
		{"[1/5] 盘点", func() int { return runInventory(ctx, stores) }},
		{"[2/5] 记录 binlog 位点", func() int { return runRecordPosition(ctx, stores, opts.file, opts.pos) }},
		{"[3/5] 回填未完成任务", func() int { return runBackfill(ctx, stores, opts.baseline, opts.batch) }},
		{"[4/5] 标记缓存基线", func() int { return runMarkCacheBaseline(ctx, stores) }},
		{"[5/5] 核对", func() int { return runVerify(ctx, stores, opts.baseline) }},
	}
	for _, step := range steps {
		fmt.Printf("\n===== %s =====\n", step.title)
		if code := step.run(); code != exitOK {
			fmt.Fprintf(os.Stderr, "rag-cdc-cutover: 步骤 %q 失败（退出码 %d），后续步骤未执行\n", step.title, code)
			return code
		}
	}
	fmt.Println("\n--all 完成：位点、回填、缓存基线与核对全部通过。")
	return exitOK
}

// trimSpace trims whitespace without importing strings for one call site.
func trimSpace(value string) string {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\t' || value[start] == '\n' || value[start] == '\r') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t' || value[end-1] == '\n' || value[end-1] == '\r') {
		end--
	}
	return value[start:end]
}
