package manager

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

type DefaultManager struct {
	config   config.Config
	store    store.Store
	provider provider.Provider
	// Serialize reuse and removal of object references within this manager.
	objectReferencesMu sync.Mutex
	// limiter 用 atomic.Value 持有而不是裸字段：SetRateLimiter 是导出方法，
	// 约定在启动期调用，但约定挡不住运行期调用——那是一次真实的数据竞争
	// （-race 直接报 applyRateLimit 读 / SetRateLimiter 写）。
	// 原子替换的成本几乎为零，没有理由把正确性寄托在调用时机上。
	limiter atomic.Value

	// signer 是配置选定的签名模式；ossSigner 始终是 provider 自带的那一档。
	// 两个都留着是因为 private 资源永远不接受无法绑定用户的模式，
	// 需要一个确定的回落目标（见 signerFor）。
	signer    URLSigner
	ossSigner URLSigner
	// quota 为空表示部署没有接配额存储，用量维护与限额检查一并跳过。
	quota store.QuotaStore
	// metrics 与 limiter 同样用 atomic.Value：SetMetrics 是导出方法，
	// 约定启动期调用挡不住运行期调用。
	metrics atomic.Value
	// audit 同上。
	audit atomic.Value
	// inspector 同上。零值表示没有注入，检视整条跳过。
	inspector atomic.Value
	// cdn 同上。零值表示没有注入 CDN 刷新实现。
	cdn atomic.Value
	// replica 同上。零值表示没有配置副本目标。
	replica atomic.Value
	// cdnLogs 同上。零值表示没有注入边缘日志回捞实现。
	cdnLogs atomic.Value
}

func NewDefaultManager(cfg config.Config, st store.Store, p provider.Provider) (*DefaultManager, error) {
	if st == nil {
		return nil, fmt.Errorf("resource store is nil")
	}
	if p == nil {
		return nil, fmt.Errorf("resource provider is nil")
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m := &DefaultManager{config: cfg, store: st, provider: p}
	m.ossSigner = &ossSigner{
		p:      p,
		secret: cfg.Access.SignedURLSecret,
		scheme: cfg.Provider.Scheme,
		host:   cfg.Provider.Domain,
	}
	m.signer = m.ossSigner
	switch cfg.Access.URLMode {
	case config.URLModeCDNAuthA, config.URLModeCDNAuthB, config.URLModeCDNAuthC:
		m.signer = &cdnAuthSigner{
			mode:   cfg.Access.URLMode,
			key:    cfg.Access.CDNAuthKey,
			uid:    cfg.Access.CDNAuthUID,
			scheme: cfg.Provider.Scheme,
			host:   orDefault(cfg.Access.CDNDomain, cfg.Provider.Domain),
			p:      p,
		}
	}
	// 如果 quota 存储恰好就是主 Store 的实现（MongoStore 同时满足两个接口），
	// 直接复用，调用方不必再注入一次。
	if qs, ok := st.(store.QuotaStore); ok {
		m.quota = qs
	}
	m.SetMetrics(nil)
	m.SetAuditSink(nil)
	m.SetRateLimiter(newFixedWindowLimiter(time.Duration(cfg.Access.RateLimitWindowSeconds) * time.Second))
	return m, nil
}

// Provider 返回底层 provider，供 transport 层注册验签路由等场景使用。
func (m *DefaultManager) Provider() provider.Provider {
	return m.provider
}
