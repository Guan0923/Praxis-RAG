package redisstate

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// resultSnapshotIndexSuffix 是结果快照索引 key 在前缀之后的部分。
// 完整的 key 形如 rag:state:v1:index:search-results。
const resultSnapshotIndexSuffix = ":index:search-results"

// ResultSnapshotIndex 用 Redis ZSET(score=expires_at) 记录有效结果快照身份。
//
// 快照本身存放在 transient 里，key 是 <prefix>:transient:{sha256} 哈希形式，
// 无法用 SCAN 前缀区分（同一个哈希 key 空间还混有队列任务等其它类型），所以
// 监控页无法靠扫描统计有效快照数；这里额外维护"身份 -> 过期时间"的索引：
// Add 写入成员，CountActive 先清掉 score<=now 的过期成员再 ZCARD 计数。
//
// 索引只是可选的观测数据：写入失败不会影响快照保存（见
// state.ResultSnapshots.Save），Redis 未启用或不可用时读取返回
// *TransientUnavailable，供监控页显示"未知"。
type ResultSnapshotIndex struct {
	client *Client
}

// 编译期断言：ResultSnapshotIndex 可以直接装配到 state.ResultSnapshots.Index。
var _ state.SnapshotIndex = (*ResultSnapshotIndex)(nil)

// ResultSnapshotIndex 返回本后端的有效结果快照索引视图。
func (c *Client) ResultSnapshotIndex() *ResultSnapshotIndex {
	return &ResultSnapshotIndex{client: c}
}

// Key 返回索引 ZSET 的 Redis key（tests/diagnostics）。
func (i *ResultSnapshotIndex) Key() string {
	prefix := state.DefaultCacheSettings().Prefix
	if i != nil && i.client != nil && i.client.settings.Prefix != "" {
		prefix = i.client.settings.Prefix
	}
	return prefix + resultSnapshotIndexSuffix
}

// connection 与 Transient.connection 一样：未启用时报告"需要 Redis"，
// 连接不可用时报告索引不可用；两类错误都不包含 URL 或凭据。
func (i *ResultSnapshotIndex) connection() (*redis.Client, error) {
	if i == nil || i.client == nil || !i.client.Enabled() {
		return nil, newTransientUnavailable("Redis is required for queued operations")
	}
	connection, err := i.client.connection()
	if err != nil {
		return nil, newTransientUnavailable("Redis result snapshot index unavailable")
	}
	return connection, nil
}

// Add 把一条结果快照身份写入索引，score 是它的过期时间（Unix 秒，可带小数）。
// identity 为空或 expiresAt 不是有限数时返回 *state.ValueError。
func (i *ResultSnapshotIndex) Add(ctx context.Context, identity string, expiresAt float64) error {
	if identity == "" {
		return &state.ValueError{Message: "Redis result snapshot identity must not be empty"}
	}
	if math.IsNaN(expiresAt) || math.IsInf(expiresAt, 0) {
		return &state.ValueError{Message: "Redis result snapshot expiry must be finite"}
	}
	connection, err := i.connection()
	if err != nil {
		return err
	}
	if err := connection.ZAdd(ctx, i.Key(), redis.Z{Score: expiresAt, Member: identity}).Err(); err != nil {
		return newTransientUnavailable("Redis result snapshot index unavailable")
	}
	return nil
}

// AddSnapshot 实现 state.SnapshotIndex，是 Add 的适配器命名。
func (i *ResultSnapshotIndex) AddSnapshot(ctx context.Context, identity string, expiresAt float64) error {
	return i.Add(ctx, identity, expiresAt)
}

// CountActive 返回 now 时刻仍然有效的结果快照数：先 ZREMRANGEBYSCORE 清理
// score<=now 的过期成员，再 ZCARD 计数。Redis 未启用/不可用时返回错误，
// 让监控页把该指标显示为未知而不是 0。
func (i *ResultSnapshotIndex) CountActive(ctx context.Context, now time.Time) (int, error) {
	connection, err := i.connection()
	if err != nil {
		return 0, err
	}
	key := i.Key()
	// 把 now 转成带小数的 Unix 秒，避免分数秒过期成员被晚清理。
	cutoff := strconv.FormatFloat(float64(now.Unix())+float64(now.Nanosecond())/float64(time.Second), 'f', -1, 64)
	if err := connection.ZRemRangeByScore(ctx, key, "-inf", cutoff).Err(); err != nil {
		return 0, newTransientUnavailable("Redis result snapshot index unavailable")
	}
	count, err := connection.ZCard(ctx, key).Result()
	if err != nil {
		return 0, newTransientUnavailable("Redis result snapshot index unavailable")
	}
	return int(count), nil
}
