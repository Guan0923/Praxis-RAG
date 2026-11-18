package monitor

// This file holds the dependency probe of the optional LightRAG knowledge-graph
// sidecar (python-workers/lightrag-service, HTTP :8093). It reads GET /healthz
// through internal/lightrag and renders the same dependency row shape as MySQL,
// Redis, Qdrant, Kafka and pdf-service. The probe is read-only and never prints
// the configured URL (which may carry credentials), the shared token or any
// model API key.

import (
	"context"
	"fmt"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/lightrag"
)

// probeLightRAG checks the optional knowledge-graph sidecar.
//
// The sidecar answers GET /healthz without authentication and always returns
// HTTP 200, reporting deployment problems in config_error and the upstream
// package versions in upstream. The probe therefore reads the payload instead
// of trusting the status code. An empty LightRAGServiceURL means graph
// retrieval is not deployed: the row reports "disabled", which the overall
// three-state aggregation ignores, so an absent optional capability is never a
// fault.
//
// Parameters:
//   - ctx: bounded probe context. runProbe wraps the caller's context with
//     probeTimeout, so the health request can never delay the snapshot.
//
// Returns the dependency result: disabled without a URL, error when the sidecar
// is unreachable, answers non-2xx or reports an unrecognized status, degraded
// when it is reachable but its payload reports an unusable configuration or a
// drifted upstream version, and ok otherwise.
func (s *Service) probeLightRAG(ctx context.Context) probeResult {
	baseURL := strings.TrimSpace(s.options.LightRAGServiceURL)
	if baseURL == "" {
		return probeResult{status: statusDisabled, detail: "未配置 LightRAG 侧车：图谱检索未启用"}
	}
	client, err := lightrag.NewClient(lightrag.Options{
		BaseURL:    baseURL,
		Token:      s.options.LightRAGServiceToken,
		Timeout:    s.options.LightRAGServiceTimeout,
		HTTPClient: s.httpClient,
	})
	if err != nil {
		// NewClient validates the address without echoing it, so a
		// credential embedded in the value cannot reach the snapshot.
		return probeResult{
			status: statusError,
			detail: "LightRAG 侧车地址无效：" + boundedText(err.Error(), 120),
		}
	}
	health, err := client.Health(ctx)
	if err != nil {
		// Every client failure is a short Chinese sentence: the token is
		// stripped from the server detail and the URL is never included.
		return probeResult{status: statusError, detail: boundedText(err.Error(), 160)}
	}
	if health == nil {
		return probeResult{status: statusError, detail: "LightRAG 健康检查返回空结果"}
	}
	return lightragHealthResult(health)
}

// lightragHealthResult turns one /healthz payload into a dependency result.
//
// The sidecar always answers HTTP 200, so the payload decides the state: a
// non-empty config_error means the process runs but cannot serve graph
// retrieval (for example the pinned package is missing) and degrades like the
// pdf-service converter check; an installed upstream version that differs from
// the pinned one is an image drift and also degrades, so a stale sidecar is
// visible without reading logs. The structured upstream report is attached to
// every outcome so the drift stays machine-readable.
//
// Parameters:
//   - health: decoded /healthz payload.
//
// Returns the dependency result for the "lightrag" row.
func lightragHealthResult(health *lightrag.HealthStatus) probeResult {
	upstream := lightragUpstreamView(health.Upstream)
	switch strings.ToLower(strings.TrimSpace(health.Status)) {
	case "ok":
		if configError := strings.TrimSpace(health.ConfigError); configError != "" {
			return probeResult{
				status:   statusDegraded,
				detail:   "配置不可用：" + boundedText(configError, 120),
				upstream: upstream,
			}
		}
		if upstream != nil && upstream.Drift {
			return probeResult{
				status:   statusDegraded,
				detail:   "上游版本漂移：" + lightragUpstreamText(*upstream),
				upstream: upstream,
			}
		}
		return probeResult{status: statusOK, detail: lightragHealthDetail(health, upstream), upstream: upstream}
	case "degraded":
		return probeResult{status: statusDegraded, detail: lightragHealthDetail(health, upstream), upstream: upstream}
	case "":
		return probeResult{status: statusDegraded, detail: "健康检查未返回状态", upstream: upstream}
	default:
		return probeResult{
			status:   statusError,
			detail:   "LightRAG 服务状态为 " + boundedText(health.Status, 40),
			upstream: upstream,
		}
	}
}

// lightragHealthDetail renders the human-readable row detail: the open
// workspace count, the sidecar build version and the upstream package version
// pair.
//
// Parameters:
//   - health: decoded /healthz payload.
//   - upstream: normalized upstream report; nil when the sidecar names no
//     package and the detail must omit the upstream sentence.
//
// Returns one bounded Chinese detail line used by the Console dependency row.
func lightragHealthDetail(health *lightrag.HealthStatus, upstream *dependencyUpstream) string {
	parts := make([]string, 0, 3)
	parts = append(parts, fmt.Sprintf("工作区 %d 个", health.Workspaces))
	if version := strings.TrimSpace(health.Version); version != "" {
		parts = append(parts, "版本 "+boundedText(version, 40))
	}
	if upstream != nil {
		parts = append(parts, lightragUpstreamText(*upstream))
	}
	return strings.Join(parts, "，")
}

// lightragUpstreamText renders the upstream package/version summary used both
// by the healthy detail line and by the drift detail line.
//
// Parameters:
//   - upstream: normalized upstream report, never nil.
//
// Returns a Chinese phrase: "上游 <pkg> 未安装", "<pkg> 期望 <pinned>、实际
// <installed>" when the versions drifted, or "上游 <pkg> <installed>（与 pin
// 一致）" when they match.
func lightragUpstreamText(upstream dependencyUpstream) string {
	pkg := boundedText(upstream.Package, 40)
	switch {
	case upstream.Installed == "":
		return "上游 " + pkg + " 未安装"
	case upstream.Drift:
		return fmt.Sprintf("%s 期望 %s、实际 %s",
			pkg, boundedText(upstream.Expected, 40), boundedText(upstream.Installed, 40))
	default:
		return "上游 " + pkg + " " + boundedText(upstream.Installed, 40) + "（与 pin 一致）"
	}
}

// lightragUpstreamView normalizes the upstream version report of /healthz.
//
// Parameters:
//   - upstream: upstream block of the health payload.
//
// Returns nil when the sidecar names no package, so a dependency without a
// versioned upstream keeps the exact JSON shape of the existing rows (the
// additive "upstream" field stays omitted); otherwise the package name, the
// pinned (expected) and installed (actual) versions. Drift is only true when
// both versions are known and differ, so an unknown installed version is not
// reported as drift.
func lightragUpstreamView(upstream lightrag.UpstreamVersion) *dependencyUpstream {
	pkg := strings.TrimSpace(upstream.Package)
	if pkg == "" {
		return nil
	}
	expected := lightragBoundedField(upstream.Pinned)
	installed := lightragBoundedField(upstream.Installed)
	return &dependencyUpstream{
		Package:   boundedText(pkg, 40),
		Expected:  expected,
		Installed: installed,
		Drift:     expected != "" && installed != "" && expected != installed,
	}
}

// lightragBoundedField trims and caps one sidecar-provided version field while
// keeping a missing value empty, so drift detection can tell "unknown" from a
// real version instead of receiving the boundedText placeholder.
//
// Parameters:
//   - value: raw field value from the /healthz payload.
//
// Returns the trimmed, rune-capped value, or "" when the value was missing.
func lightragBoundedField(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return boundedText(trimmed, 40)
}
