// Command rag-connect operates the distributed Kafka Connect cluster that hosts
// the two Debezium MySQL capture connectors.
//
// Usage:
//
//	rag-connect register [--urls ...] [--connector-dir DIR] [--expected-version V]
//	rag-connect wait     [--timeout 180s] [--poll 2s]
//	rag-connect status   [--json]
//	rag-connect plugins  [--expected-version V]
//	rag-connect supervise
//	rag-connect decode   --kind document|cache [--input FILE] [--op d] [--limit N]
//
// register is idempotent: it creates a missing connector by fixed name and only
// updates a connector whose stored configuration differs, so repeated stack
// starts never bounce a healthy capture task. wait blocks until both named
// connectors and all their tasks are RUNNING. supervise runs the failed-task
// restart loop that Kafka Connect itself does not provide. decode feeds real
// Kafka records through the same decoders the Go workers use (internal/mq), so
// an acceptance run verifies the deployed message shape instead of a fixture.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/connect"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// Exit codes follow the repository convention: 0 success, 1 runtime failure,
// 2 configuration/usage error.
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 2
)

// defaultConnectorDir is where the declarative templates live in the repository
// (and at /app/deploy/kafka-connect/connectors in the container image).
const defaultConnectorDir = "deploy/kafka-connect/connectors"

func main() { os.Exit(run(os.Args[1:])) }

// usageError marks configuration/usage failures so main can exit 2.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rag-connect <register|wait|status|plugins|supervise|decode> [flags]")
		return exitConfig
	}
	command := args[0]
	flags := flag.NewFlagSet("rag-connect "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	var err error
	switch command {
	case "register":
		err = runRegister(flags, args[1:])
	case "wait":
		err = runWait(flags, args[1:])
	case "status":
		err = runStatus(flags, args[1:])
	case "plugins":
		err = runPlugins(flags, args[1:])
	case "supervise":
		err = runSupervise(flags, args[1:])
	case "decode":
		err = runDecode(flags, args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, "usage: rag-connect <register|wait|status|plugins|supervise|decode> [flags]")
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "rag-connect: unknown command %q\n", command)
		fmt.Fprintln(os.Stderr, "usage: rag-connect <register|wait|status|plugins|supervise|decode> [flags]")
		return exitConfig
	}
	if err == nil {
		return exitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "rag-connect %s: %v\n", command, err)
	var usage *usageError
	if errors.As(err, &usage) {
		return exitConfig
	}
	return exitFailure
}

// commonFlags adds the REST endpoint override shared by every REST command.
func commonFlags(flags *flag.FlagSet, urls *string, timeout *time.Duration) {
	flags.StringVar(urls, "urls", "", "Kafka Connect REST 地址列表（逗号分隔，默认 RAG_CONNECT_URLS）")
	flags.DurationVar(timeout, "timeout", 0, "单次 REST 超时（默认 RAG_CONNECT_TIMEOUT_SECONDS 或 3s）")
}

// resolveSettings applies flag overrides on top of the environment settings.
func resolveSettings(urls string, timeout time.Duration) (connect.Settings, error) {
	settings, err := connect.SettingsFromEnv()
	if err != nil {
		return connect.Settings{}, &usageError{err: err}
	}
	if strings.TrimSpace(urls) != "" {
		parsed, parseErr := connect.ParseURLs(urls)
		if parseErr != nil {
			return connect.Settings{}, &usageError{err: parseErr}
		}
		if len(parsed) == 0 {
			return connect.Settings{}, &usageError{err: errors.New("--urls 不能为空")}
		}
		settings.URLs = parsed
		settings.SupervisorEnabled = true
	}
	if timeout > 0 {
		settings.Timeout = timeout
	}
	return settings, nil
}

func timeoutOrDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

// splitList exists for flags that accept a comma-separated list; it keeps the
// parsing identical to SettingsFromEnv without exposing an internal helper.
func splitList(raw string) []string {
	items := make([]string, 0, 2)
	for _, item := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

// runRegister verifies the plugin version on every reachable worker, loads the
// two templates and upserts them by fixed name.
func runRegister(flags *flag.FlagSet, args []string) error {
	var (
		urls            string
		timeout         time.Duration
		connectorDir    string
		expectedVersion string
		wait            time.Duration
		offline         bool
	)
	commonFlags(flags, &urls, &timeout)
	flags.StringVar(&connectorDir, "connector-dir", "", "连接器模板目录（默认 RAG_CONNECT_CONNECTOR_DIR 或 deploy/kafka-connect/connectors）")
	flags.StringVar(&expectedVersion, "expected-version", "", "期望的 Debezium MySQL Connector 版本（默认 DEBEZIUM_CONNECT_VERSION）")
	flags.DurationVar(&wait, "wait", 120*time.Second, "等待所有 Connect worker 就绪的最长时间")
	flags.BoolVar(&offline, "offline", false, "只展开模板并打印结果，不访问 Kafka Connect")
	if err := flags.Parse(args); err != nil {
		return err
	}
	settings, err := resolveSettings(urls, timeout)
	if err != nil {
		return err
	}
	if strings.TrimSpace(connectorDir) == "" {
		connectorDir = strings.TrimSpace(os.Getenv("RAG_CONNECT_CONNECTOR_DIR"))
	}
	if strings.TrimSpace(connectorDir) == "" {
		connectorDir = defaultConnectorDir
	}
	if strings.TrimSpace(expectedVersion) == "" {
		expectedVersion = settings.ExpectedVersion
	}
	specs, err := connect.LoadConnectorDir(connectorDir, os.Getenv)
	if err != nil {
		return &usageError{err: err}
	}
	if err := checkTemplateNames(specs, settings); err != nil {
		return &usageError{err: err}
	}
	if offline {
		for _, spec := range specs {
			fmt.Printf("%s (%s): %d 项配置已展开\n", spec.Name, spec.Source, len(spec.Config))
		}
		return nil
	}
	if !settings.Configured() {
		return &usageError{err: errors.New("未配置 Kafka Connect REST 地址（RAG_CONNECT_URLS 或 --urls）")}
	}
	client, err := connect.NewClient(settings.URLs, settings.Timeout)
	if err != nil {
		return &usageError{err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), max(wait, settings.Timeout*4))
	defer cancel()
	versions, err := waitForPlugins(ctx, client, expectedVersion, wait)
	if err != nil {
		return fmt.Errorf("校验 Debezium MySQL Connector 版本失败：%w", err)
	}
	origins := make([]string, 0, len(versions))
	for origin, version := range versions {
		origins = append(origins, origin+"="+version)
	}
	fmt.Printf("Kafka Connect worker 插件版本：%s\n", strings.Join(origins, "，"))
	for _, spec := range specs {
		result, upsertErr := client.Upsert(ctx, spec.Name, spec.Config)
		if upsertErr != nil {
			return fmt.Errorf("注册连接器 %s 失败：%w", spec.Name, upsertErr)
		}
		switch {
		case result.Created:
			fmt.Printf("已创建连接器 %s（%s）\n", result.Name, spec.Source)
		case result.Updated:
			fmt.Printf("已更新连接器 %s 的配置（%s）\n", result.Name, spec.Source)
		default:
			fmt.Printf("连接器 %s 配置未变化，保持运行（%s）\n", result.Name, spec.Source)
		}
	}
	return nil
}

// waitForPlugins retries the plugin inventory until every configured worker
// answers and reports the expected MySQL connector version. Compose starts the
// workers concurrently with connect-init, so a single attempt would race.
func waitForPlugins(ctx context.Context, client *connect.Client, expectedVersion string, wait time.Duration) (map[string]string, error) {
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		versions, err := connect.VerifyConnectorPlugins(ctx, client, expectedVersion)
		if err == nil {
			return versions, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(2 * time.Second):
		}
	}
}

// checkTemplateNames refuses a template directory that would register a
// connector the monitor/supervisor does not watch.
func checkTemplateNames(specs []connect.ConnectorSpec, settings connect.Settings) error {
	names := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		names[spec.Name] = struct{}{}
	}
	for _, expected := range []string{settings.DocumentConnector, settings.CacheConnector} {
		if _, ok := names[expected]; !ok {
			return fmt.Errorf("模板目录缺少连接器 %q（检查文件名/name 占位符与 RAG_CONNECT_DOCUMENT_CONNECTOR/RAG_CONNECT_CACHE_CONNECTOR）", expected)
		}
	}
	if len(names) != 2 {
		return fmt.Errorf("模板目录必须恰好声明两个连接器，实际 %d 个", len(names))
	}
	return nil
}

// runWait blocks until both fixed connectors and their tasks are RUNNING.
func runWait(flags *flag.FlagSet, args []string) error {
	var (
		urls    string
		timeout time.Duration
		poll    time.Duration
		wait    time.Duration
	)
	commonFlags(flags, &urls, &timeout)
	flags.DurationVar(&wait, "wait", 180*time.Second, "等待连接器就绪的总时长")
	flags.DurationVar(&poll, "poll", 2*time.Second, "轮询间隔")
	if err := flags.Parse(args); err != nil {
		return err
	}
	settings, err := resolveSettings(urls, timeout)
	if err != nil {
		return err
	}
	if !settings.Configured() {
		return &usageError{err: errors.New("未配置 Kafka Connect REST 地址（RAG_CONNECT_URLS 或 --urls）")}
	}
	client, err := connect.NewClient(settings.URLs, settings.Timeout)
	if err != nil {
		return &usageError{err: err}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := connect.WaitForReady(ctx, client, []string{settings.DocumentConnector, settings.CacheConnector}, wait, poll); err != nil {
		return err
	}
	for _, name := range []string{settings.DocumentConnector, settings.CacheConnector} {
		status, statusErr := client.Status(ctx, name)
		if statusErr != nil {
			return statusErr
		}
		health := connect.ConnectorHealth{
			Connector: status.Name,
			State:     status.ConnectorState,
			WorkerID:  status.ConnectorWorker,
		}
		for _, task := range status.Tasks {
			health.Tasks = append(health.Tasks, connect.TaskHealth{
				ID: task.ID, State: task.State, WorkerID: task.WorkerID, Trace: task.Trace,
			})
		}
		fmt.Println("就绪：" + health.Describe())
	}
	return nil
}

// runStatus prints the live state of both connectors. --json emits the health
// documents the monitor consumes (minus service-only fields).
func runStatus(flags *flag.FlagSet, args []string) error {
	var (
		urls    string
		timeout time.Duration
		asJSON  bool
		all     bool
	)
	commonFlags(flags, &urls, &timeout)
	flags.BoolVar(&asJSON, "json", false, "以 JSON 输出")
	flags.BoolVar(&all, "all", false, "连同注册在集群里的其他连接器一起列出")
	if err := flags.Parse(args); err != nil {
		return err
	}
	settings, err := resolveSettings(urls, timeout)
	if err != nil {
		return err
	}
	if !settings.Configured() {
		return &usageError{err: errors.New("未配置 Kafka Connect REST 地址（RAG_CONNECT_URLS 或 --urls）")}
	}
	client, err := connect.NewClient(settings.URLs, settings.Timeout)
	if err != nil {
		return &usageError{err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), max(settings.Timeout*4, 15*time.Second))
	defer cancel()
	runtime, err := connect.NewRuntime(settings, nil)
	if err != nil {
		return err
	}
	connectors := runtime.ConnectorNames()
	if all {
		names, namesErr := client.ConnectorNames(ctx)
		if namesErr != nil {
			return namesErr
		}
		seen := make(map[string]struct{}, len(names))
		for _, name := range connectors {
			seen[name] = struct{}{}
		}
		for _, name := range names {
			if _, ok := seen[name]; !ok {
				connectors = append(connectors, name)
			}
		}
	}
	healths := make([]connect.ConnectorHealth, 0, len(connectors))
	for _, name := range connectors {
		health, healthErr := runtime.Health(ctx, name)
		if healthErr != nil {
			return fmt.Errorf("读取连接器 %s 状态失败：%w", name, healthErr)
		}
		healths = append(healths, health)
	}
	if asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(healths)
	}
	for _, health := range healths {
		fmt.Println(health.Describe())
		if health.FailureReason != "" {
			fmt.Println("  失败原因：" + health.FailureReason)
		}
	}
	return nil
}

// runPlugins verifies the Debezium MySQL connector version on every worker.
func runPlugins(flags *flag.FlagSet, args []string) error {
	var (
		urls            string
		timeout         time.Duration
		expectedVersion string
	)
	commonFlags(flags, &urls, &timeout)
	flags.StringVar(&expectedVersion, "expected-version", "", "期望版本（默认 DEBEZIUM_CONNECT_VERSION）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	settings, err := resolveSettings(urls, timeout)
	if err != nil {
		return err
	}
	if !settings.Configured() {
		return &usageError{err: errors.New("未配置 Kafka Connect REST 地址（RAG_CONNECT_URLS 或 --urls）")}
	}
	if strings.TrimSpace(expectedVersion) == "" {
		expectedVersion = settings.ExpectedVersion
	}
	client, err := connect.NewClient(settings.URLs, settings.Timeout)
	if err != nil {
		return &usageError{err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), max(settings.Timeout*4, 30*time.Second))
	defer cancel()
	versions, err := connect.VerifyConnectorPlugins(ctx, client, expectedVersion)
	if err != nil {
		return err
	}
	for origin, version := range versions {
		fmt.Printf("%s: %s=%s\n", origin, connect.MySQLConnectorClass, version)
	}
	return nil
}

// runSupervise runs the FAILED-task restart loop in the foreground. Deployments
// without rag-service (for example a dedicated operations host) can run it
// under their process manager.
func runSupervise(flags *flag.FlagSet, args []string) error {
	var (
		urls    string
		timeout time.Duration
	)
	commonFlags(flags, &urls, &timeout)
	if err := flags.Parse(args); err != nil {
		return err
	}
	settings, err := resolveSettings(urls, timeout)
	if err != nil {
		return err
	}
	if !settings.Configured() {
		return &usageError{err: errors.New("未配置 Kafka Connect REST 地址（RAG_CONNECT_URLS 或 --urls）")}
	}
	if !settings.SupervisorEnabled {
		return &usageError{err: errors.New("RAG_CONNECT_SUPERVISOR_ENABLED=false 时不能运行 supervise")}
	}
	logger, err := logging.Setup()
	if err != nil {
		return err
	}
	defer func() { _ = logging.CloseLogFile() }()
	runtime, err := connect.NewRuntime(settings, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime.Start(ctx)
	<-ctx.Done()
	runtime.Close()
	logger.Info("Kafka Connect 失败任务自动重启已停止")
	return nil
}

// decodeOutput is the stable JSON line the acceptance script asserts on.
type decodeOutput struct {
	Kind       string         `json:"kind"`
	Table      string         `json:"table,omitempty"`
	Op         string         `json:"op,omitempty"`
	ArtifactID string         `json:"artifact_id,omitempty"`
	Namespace  string         `json:"namespace,omitempty"`
	Schedule   int64          `json:"schedule_version,omitempty"`
	Before     map[string]any `json:"before,omitempty"`
	After      map[string]any `json:"after,omitempty"`
}

// runDecode feeds newline-delimited Kafka values through internal/mq. The
// decoders are the production ones, so a passing decode proves the deployed
// converter/routing still matches the Go workers.
func runDecode(flags *flag.FlagSet, args []string) error {
	var (
		kind  string
		input string
		op    string
		limit int
	)
	flags.StringVar(&kind, "kind", "", "消息类型：document 或 cache")
	flags.StringVar(&input, "input", "", "输入文件（每行一个 Kafka value）；默认标准输入")
	flags.StringVar(&op, "op", "", "只输出指定 op（仅 cache：c/u/r/d）")
	flags.IntVar(&limit, "limit", 0, "最多输出多少条（0 表示全部）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "document" && kind != "cache" {
		return usagef("--kind 必须是 document 或 cache")
	}
	if strings.TrimSpace(op) != "" && kind != "cache" {
		return usagef("--op 只适用于 cache 消息")
	}
	if limit < 0 {
		return usagef("--limit 必须是非负整数")
	}
	var reader io.Reader = os.Stdin
	if strings.TrimSpace(input) != "" {
		file, err := os.Open(input)
		if err != nil {
			return fmt.Errorf("打开输入文件失败：%w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	decoded := 0
	ignored := 0
	malformed := 0
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		output, err := decodeLine(kind, []byte(line))
		if err != nil {
			if state.IgnorableCDCChange(err) {
				ignored++
				continue
			}
			malformed++
			fmt.Fprintf(os.Stderr, "跳过无法解码的消息：%v\n", bounded(err.Error(), 160))
			continue
		}
		if op != "" && output.Op != op {
			continue
		}
		if err := encoder.Encode(output); err != nil {
			return err
		}
		decoded++
		if limit > 0 && decoded >= limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取输入失败：%w", err)
	}
	fmt.Fprintf(os.Stderr, "decoded=%d ignored=%d malformed=%d\n", decoded, ignored, malformed)
	if decoded == 0 {
		return fmt.Errorf("没有解码出任何 %s 消息", kind)
	}
	return nil
}

// decodeLine decodes one Kafka value with the production decoder.
func decodeLine(kind string, body []byte) (decodeOutput, error) {
	if kind == "document" {
		notification, err := mq.DecodeDocumentNotification(body)
		if err != nil {
			return decodeOutput{}, err
		}
		return decodeOutput{
			Kind:       "document",
			ArtifactID: notification.ArtifactID,
			Namespace:  notification.Namespace,
			Schedule:   notification.ScheduleVersion,
		}, nil
	}
	change, err := mq.DecodeCacheChange(body)
	if err != nil {
		return decodeOutput{}, err
	}
	return decodeOutput{
		Kind:   "cache",
		Table:  change.Table,
		Op:     change.Op,
		Before: change.Before,
		After:  change.After,
	}, nil
}

// bounded truncates a diagnostic string for stderr output.
func bounded(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
