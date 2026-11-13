package monitor

import (
	"context"
	"errors"
	"testing"
)

// TestMonitorPipelineCache covers the cache read-model convergence row: ok when
// both counters are zero, degraded when dirty keys or failed refreshes remain,
// and unknown (JSON null numbers) when the state aggregate is unavailable.
func TestMonitorPipelineCache(t *testing.T) {
	t.Run("收敛完成", func(t *testing.T) {
		stats := sampleStats()
		stats.CacheDirtyKeys = 0
		stats.CacheRefreshFailures = 0
		overview := New(pipelineTestOptions(stats)).MonitorOverview(context.Background())
		view := pipelineOf(t, overview)
		if view.Cache.Status != statusOK || view.Cache.DirtyKeys == nil ||
			*view.Cache.DirtyKeys != 0 || view.Cache.RefreshFailures == nil ||
			*view.Cache.RefreshFailures != 0 {
			t.Fatalf("pipeline.cache = %+v", view.Cache)
		}
		if overview["status"] != statusOK {
			t.Fatalf("overall status = %v, want ok", overview["status"])
		}
	})
	t.Run("存在脏缓存", func(t *testing.T) {
		stats := sampleStats()
		stats.CacheDirtyKeys = 3
		stats.CacheRefreshFailures = 1
		overview := New(pipelineTestOptions(stats)).MonitorOverview(context.Background())
		view := pipelineOf(t, overview)
		if view.Cache.Status != statusDegraded || view.Cache.DirtyKeys == nil ||
			*view.Cache.DirtyKeys != 3 || view.Cache.RefreshFailures == nil ||
			*view.Cache.RefreshFailures != 1 {
			t.Fatalf("pipeline.cache = %+v", view.Cache)
		}
		// Dirty keys converge through the retry watermark: degraded, not fatal.
		if overview["status"] != statusDegraded {
			t.Fatalf("overall status = %v, want degraded", overview["status"])
		}
	})
	t.Run("统计不可用", func(t *testing.T) {
		overview := New(Options{
			Stats:     &fakeStatsProvider{err: errors.New("state aggregation failed")},
			MySQL:     fakeMySQLPinger{},
			PDFEngine: "go",
		}).MonitorOverview(context.Background())
		view := pipelineOf(t, overview)
		if view.Cache.Status != statusUnknown || view.Cache.DirtyKeys != nil ||
			view.Cache.RefreshFailures != nil {
			t.Fatalf("pipeline.cache = %+v, want unknown/null", view.Cache)
		}
	})
}
