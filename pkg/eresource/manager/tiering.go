package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/cdn"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// TransitionStorageClass 把一批资源的底层对象转到指定存储类型。
//
// 按 Query 选目标而不是按时间一刀切，是这个方法存在的全部理由：
// 「90 天没改过的转低频」用 bucket 生命周期规则就能做，根本不需要代码；
// 需要代码的是「素材库转低频、合同附件不动」这类只有元数据才知道的维度。
//
// 单条失败不中断整批。返回成功转换的条数。
func (m *DefaultManager) TransitionStorageClass(ctx context.Context, query *model.Query, storageClass string) (int, error) {
	setter, ok := m.provider.(provider.StorageClassSetter)
	if !ok {
		return 0, provider.ErrStorageClassUnsupported
	}
	if query == nil {
		query = &model.Query{}
	}
	// 只转 active 的资源：已软删的对象正在等保留期到期被删掉，
	// 为它们付一次转换费（OSS 的存储类型转换按请求计费）没有意义。
	query.Status = model.ResourceStatusActive
	resources, err := m.store.List(ctx, query)
	if err != nil {
		return 0, err
	}
	moved := 0
	seen := make(map[string]struct{}, len(resources))
	var errs []error
	for _, resource := range resources {
		// 去重策略下多条记录共享一个对象，转一次就够了。
		if _, done := seen[resource.ObjectKey]; done {
			continue
		}
		seen[resource.ObjectKey] = struct{}{}
		if err = setter.SetStorageClass(ctx, resource.ObjectKey, storageClass); err != nil {
			errs = append(errs, fmt.Errorf("transition %s: %w", resource.ObjectKey, err))
			continue
		}
		moved++
	}
	return moved, errors.Join(errs...)
}

// TieringPolicy 一条存储分层策略：「满足这些条件的资源转到这个存储类型」。
//
// 这是 §3「按条件选目标」的具体表达。为什么不做成 bucket 生命周期规则：
// 那边只认对象前缀与年龄，表达不了 biz_type / 租户 / 标签这些
// **只有元数据才知道**的维度。而「素材库转低频、合同附件原地不动」
// 恰恰是最常见的诉求。
//
// 能用生命周期规则做的（纯按年龄、按前缀）就该用它——
// 那是对象存储原生能力，不占业务资源，也不会因为业务进程没跑而停摆。
type TieringPolicy struct {
	// Name 策略名，只用于日志与报告，方便定位是哪条规则动了数据。
	Name string
	// Namespace / AccountID / BizType 为空表示不限定该维度。
	Namespace string
	AccountID string
	BizType   string
	// Tags 标签过滤，AND 语义。
	Tags []string
	// OlderThanDays 只处理创建时间早于 N 天的资源。
	//
	// 必须 > 0：没有年龄下限的分层策略会把刚传上来的热数据
	// 直接推进归档，而归档取回是要先解冻、按量收费的。
	OlderThanDays int
	// StorageClass 目标存储类型，见 provider.StorageClass* 常量。
	StorageClass string
	// Limit 单次处理的资源条数上限，<=0 取 200。
	Limit int64
}

// ErrInvalidTieringPolicy 分层策略不合法。
var ErrInvalidTieringPolicy = errors.New("eresource: invalid tiering policy")

// Validate 校验一条策略。
func (p *TieringPolicy) Validate() error {
	if p == nil {
		return ErrInvalidTieringPolicy
	}
	// 年龄下限是必填项而不是可选优化：缺了它，这条策略会把
	// 每一个新上传的对象立刻转走，包括正在被高频读取的那些。
	if p.OlderThanDays <= 0 {
		return fmt.Errorf("%w: %s needs older_than_days > 0", ErrInvalidTieringPolicy, p.Name)
	}
	switch p.StorageClass {
	case provider.StorageClassStandard, provider.StorageClassIA, provider.StorageClassArchive:
	default:
		return fmt.Errorf("%w: %s has unknown storage_class %q", ErrInvalidTieringPolicy, p.Name, p.StorageClass)
	}
	return nil
}

// TieringResult 一条策略的执行结果。
type TieringResult struct {
	Policy       string
	StorageClass string
	Moved        int
}

// ApplyTieringPolicies 依次执行全部分层策略。
//
// 单条策略失败不中断其余策略：一条规则命中的对象转不动
// （典型是已经在归档里的对象），不该把别的规则一起卡住。
func (m *DefaultManager) ApplyTieringPolicies(ctx context.Context, policies []TieringPolicy) ([]TieringResult, error) {
	var results []TieringResult
	var errs []error
	for i := range policies {
		policy := policies[i]
		if err := policy.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		limit := policy.Limit
		if limit <= 0 {
			limit = exportPageSize
		}
		moved, err := m.TransitionStorageClass(ctx, &model.Query{
			Namespace:     policy.Namespace,
			AccountID:     policy.AccountID,
			BizType:       policy.BizType,
			Tags:          policy.Tags,
			CreatedBefore: time.Now().AddDate(0, 0, -policy.OlderThanDays),
			Limit:         limit,
		}, policy.StorageClass)
		if err != nil {
			errs = append(errs, fmt.Errorf("tiering policy %s: %w", policy.Name, err))
		}
		if moved > 0 {
			results = append(results, TieringResult{
				Policy: policy.Name, StorageClass: policy.StorageClass, Moved: moved,
			})
		}
	}
	return results, errors.Join(errs...)
}

// PreloadCDN 把这些资源预热到 CDN 边缘。
//
// 与刷新相反的操作：刷新是「让边缘上那份立刻失效」，预热是
// 「在用户来之前先把它推到边缘」。典型场景是大促素材、新版本客户端包——
// 不预热的话，开闸瞬间全部请求一起回源，源站要扛的是完整的峰值流量。
func (m *DefaultManager) PreloadCDN(ctx context.Context, ids []string, req *model.AccessRequest) error {
	refresher := m.cdnRefresher()
	if refresher == nil {
		return ErrCDNRefresherMissing
	}
	urls, err := m.publicURLsOf(ctx, ids, req)
	if err != nil {
		return err
	}
	return refresher.Preload(ctx, urls)
}

// ErrCDNRefresherMissing 没有注入 CDN 刷新实现。
var ErrCDNRefresherMissing = errors.New("eresource: no cdn refresher configured")

// SetCDNRefresher 注入 CDN 刷新实现。约定在启动期调用。
func (m *DefaultManager) SetCDNRefresher(refresher cdn.CDNRefresher) {
	if refresher == nil {
		m.cdn.Store((*cdn.CDNRefresher)(nil))
		return
	}
	m.cdn.Store(&refresher)
}

func (m *DefaultManager) cdnRefresher() cdn.CDNRefresher {
	stored, _ := m.cdn.Load().(*cdn.CDNRefresher)
	if stored == nil {
		return nil
	}
	return *stored
}

// RefreshCDN 刷新这些资源在 CDN 边缘的缓存。
//
// 什么时候必须调它：把资源从 public 改成受控、或者删除了一个曾经公开的资源。
// 不刷新的话，边缘上那份副本在缓存过期之前照样能被任何人取到——
// 元数据里的策略已经改了，实际的可见性没有改。
//
// 刻意**不**在 Update / Delete 里自动调用：刷新是一次外部网络请求，
// 把它挂进这两个路径意味着 CDN 侧抖动会让改策略和删除一起失败。
// 什么时候刷、失败了怎么重试，交给调用方按自己的 SLA 决定。
func (m *DefaultManager) RefreshCDN(ctx context.Context, ids []string, req *model.AccessRequest) error {
	refresher := m.cdnRefresher()
	if refresher == nil {
		return ErrCDNRefresherMissing
	}
	urls, err := m.publicURLsOf(ctx, ids, req)
	if err != nil {
		return err
	}
	return refresher.Refresh(ctx, urls)
}

// publicURLsOf 把资源 ID 解析成公开直链，并逐条做租户校验。
//
// 刷新与预热处理的都是公开直链——受控资源的地址每次签发都不同，
// 边缘上根本没有一个稳定的缓存键可操作（开了时间窗对齐的除外，
// 那种情况下调用方自己知道该刷哪条）。
func (m *DefaultManager) publicURLsOf(ctx context.Context, ids []string, req *model.AccessRequest) ([]string, error) {
	urls := make([]string, 0, len(ids))
	for _, id := range ids {
		resource, err := m.store.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if err = m.authorizeScope(resource, req); err != nil {
			return nil, err
		}
		url, err := m.publicURLFor(resource.ObjectKey, resource.AccountID)
		if err != nil {
			return nil, err
		}
		urls = append(urls, url)
	}
	return urls, nil
}
