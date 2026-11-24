package manager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// ErrRateLimited 访问频率超出阈值。
var ErrRateLimited = fmt.Errorf("eresource: access rate limit exceeded")

// fixedWindowLimiter 单机固定窗口计数器。
//
// 仅适用于单实例部署与本地开发：多实例下每个实例各算各的，
// 实际放行量是阈值 × 实例数。生产环境应替换为基于 Redis 的分布式实现，
// 通过 Manager.SetRateLimiter 注入即可，无需改动调用方。
type fixedWindowLimiter struct {
	window time.Duration

	mu       sync.Mutex
	counters map[string]*windowCounter
	lastGC   time.Time
}

type windowCounter struct {
	count    int64
	expireAt time.Time
}

func newFixedWindowLimiter(window time.Duration) *fixedWindowLimiter {
	if window <= 0 {
		window = time.Minute
	}
	return &fixedWindowLimiter{
		window:   window,
		counters: make(map[string]*windowCounter),
		lastGC:   time.Now(),
	}
}

// Allow 对 key 计数并判断是否仍在阈值内。limit <= 0 表示该维度不限流。
func (l *fixedWindowLimiter) Allow(key string, limit int64) bool {
	if limit <= 0 {
		return true
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.gcLocked(now)

	counter, ok := l.counters[key]
	if !ok || now.After(counter.expireAt) {
		l.counters[key] = &windowCounter{count: 1, expireAt: now.Add(l.window)}
		return true
	}
	counter.count++
	return counter.count <= limit
}

// gcLocked 清理过期计数，避免 key 无限增长把内存吃满。
// 每经过一个窗口才扫一次，摊销后开销可忽略。
func (l *fixedWindowLimiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < l.window {
		return
	}
	l.lastGC = now
	for key, counter := range l.counters {
		if now.After(counter.expireAt) {
			delete(l.counters, key)
		}
	}
}

// RateLimiter 访问频控抽象，便于替换为 Redis 等分布式实现。
type RateLimiter interface {
	// Allow 对 key 计数并返回是否放行。limit <= 0 表示不限流。
	Allow(key string, limit int64) bool
}

// SetRateLimiter 替换默认的单机频控实现（例如换成基于 Redis 的分布式限流）。
// 可以在运行期安全调用。
func (m *DefaultManager) SetRateLimiter(limiter RateLimiter) {
	if limiter != nil {
		// 存进 atomic.Value 的必须是同一个具体类型，所以统一包一层接口值。
		m.limiter.Store(&limiter)
	}
}

// rateLimiter 读取当前生效的频控实现。
func (m *DefaultManager) rateLimiter() RateLimiter {
	stored, _ := m.limiter.Load().(*RateLimiter)
	if stored == nil {
		return nil
	}
	return *stored
}

// rateLimitDimension 一个频控维度：计数 key 与该维度的阈值。
type rateLimitDimension struct {
	name  string
	key   string
	limit int64
}

// applyRateLimit 逐维度计数。limit <= 0 表示该维度不限流；
// key 为空表示这次请求没有该维度的信息（如匿名调用方没有 UserID），同样跳过。
//
// 注意 key 为空即跳过意味着维度会静默失效：匿名访问场景下 user 维度
// 永远不生效，真正起作用的只有 ip 与 resource 维度。
func (m *DefaultManager) applyRateLimit(dimensions []rateLimitDimension) error {
	limiter := m.rateLimiter()
	if limiter == nil {
		return nil
	}
	for _, d := range dimensions {
		if d.limit <= 0 || d.key == "" {
			continue
		}
		if !limiter.Allow(d.name+":"+d.key, d.limit) {
			return fmt.Errorf("%w: dimension=%s", ErrRateLimited, d.name)
		}
	}
	return nil
}

// checkRateLimit 按 IP / 用户 / 资源三个维度做窗口计数。
// 默认实现是单机固定窗口，多实例部署应通过 SetRateLimiter 换成分布式实现。
func (m *DefaultManager) checkRateLimit(ctx context.Context, resource *model.Resource, req *model.AccessRequest) error {
	_ = ctx
	if req == nil {
		return nil
	}
	access := m.config.Access
	return m.applyRateLimit([]rateLimitDimension{
		{"ip", req.ClientIP, access.RateLimitByIP},
		{"user", req.UserID, access.RateLimitByUser},
		{"resource", resource.ID, access.RateLimitByResource},
	})
}

// AllowSignedAccess 对签名地址的下载做频控。
//
// 签名路由不查元数据、不走 verifyResourceAccess，所以防盗链与三维频控
// 都覆盖不到它：拿到一条签名地址就能在有效期内无限次拉取，是刷带宽的
// 主入口。这里按 IP 与 object key 两个维度补上计数。
//
// ip 维度与 checkRateLimit 共用同一组 key，两条下载路径合计一份配额；
// object 维度单独计数，不与按资源 ID 的 resource 维度混用——签名路由
// 只有 object key，拿不到资源 ID。
//
// 调用方必须先验签再调本方法：否则任何人都能用无效签名把某个 object
// 的配额刷满，把频控变成针对合法用户的拒绝服务手段。
func (m *DefaultManager) AllowSignedAccess(objectKey, clientIP string) error {
	access := m.config.Access
	return m.applyRateLimit([]rateLimitDimension{
		{"ip", clientIP, access.RateLimitByIP},
		{"object", objectKey, access.RateLimitByResource},
	})
}
