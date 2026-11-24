package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

// ErrQuotaExceeded 租户用量超过配额。
var ErrQuotaExceeded = errors.New("eresource: tenant storage quota exceeded")

// Usage 读取某个租户的当前用量。没有接用量存储时返回 nil。
//
// 对外导出是为了让业务侧能直接展示「这个租户用了多少」——
// 在这之前这个数字只能靠遍历 List 自己累加。
func (m *DefaultManager) Usage(ctx context.Context, namespace, accountID string) (*model.Usage, error) {
	if m.quota == nil {
		return nil, nil
	}
	return m.quota.GetUsage(ctx, namespace, accountID)
}

// checkQuota 在写入之前判断本次上传是否会撑破配额。
//
// 只在 EnableQuota 打开时拦截。关闭时仍然维护计数（addUsage 照常调用），
// 这是刻意的：先攒出真实数字再谈限额，否则第一次打开开关
// 就会把所有存量租户一起拦死，而谁也不知道阈值该设多少。
//
// 判断用的是「当前用量 + 本次体积」而不是严格的原子预留：
// 并发上传确实可能一起越过阈值，超出量以单次文件大小为界。
// 为了封死这点而引入分布式锁不划算——配额是成本治理，不是安全边界。
func (m *DefaultManager) checkQuota(ctx context.Context, namespace, accountID string, size int64) error {
	if m.quota == nil || !m.config.Manager.EnableQuota {
		return nil
	}
	maxBytes := m.config.Manager.QuotaMaxBytes
	maxFiles := m.config.Manager.QuotaMaxFiles
	if maxBytes <= 0 && maxFiles <= 0 {
		return nil
	}
	usage, err := m.quota.GetUsage(ctx, namespace, accountID)
	if err != nil {
		// 读不到用量时放行而不是拒绝：配额存储抖动不应该让上传整体不可用。
		// 这和删除侧的 fail-closed 不同——那边的代价是数据丢失，这边只是多收一点存储。
		return nil
	}
	if usage == nil {
		return nil
	}
	if maxBytes > 0 && usage.Bytes+size > maxBytes {
		return fmt.Errorf("%w: %d + %d > %d bytes", ErrQuotaExceeded, usage.Bytes, size, maxBytes)
	}
	if maxFiles > 0 && usage.Files+1 > maxFiles {
		return fmt.Errorf("%w: %d + 1 > %d files", ErrQuotaExceeded, usage.Files, maxFiles)
	}
	return nil
}

// addUsage 调整用量。失败只影响计数的准确性，不影响本次操作的结果，
// 因此刻意不返回错误——让一次 $inc 失败回滚一次已经成功的上传，
// 代价与收益完全不成比例。偏差由定时对账任务修正。
func (m *DefaultManager) addUsage(ctx context.Context, namespace, accountID string, bytes, files int64) {
	if m.quota == nil {
		return
	}
	_ = m.quota.AddUsage(ctx, namespace, accountID, bytes, files)
}

// UsageDrift 一次用量对账的结果。
type UsageDrift struct {
	Namespace string
	AccountID string
	Counted   *model.Usage // 增量计数维护的账面值
	Actual    *model.Usage // 从资源记录现算出来的真实值
	BytesDiff int64        // Actual - Counted
	FilesDiff int64
	Corrected bool // 是否已经把账面修正到真实值
}

// HasDrift 判断是否存在偏差。
func (d *UsageDrift) HasDrift() bool {
	return d != nil && (d.BytesDiff != 0 || d.FilesDiff != 0)
}

// ReconcileUsage 对账某个租户的用量，并按需把账面修正到真实值。
//
// 为什么必须有这一步：增量计数会漂移。一次 $inc 失败（addUsage 刻意
// 不返回错误——让一次计数失败回滚一次已成功的上传，代价与收益完全不成比例）、
// 一次进程在 store.Create 与 AddUsage 之间崩溃，账面就和事实差开了。
// 这种偏差只会累积不会自愈，而配额是拿这个数去拦上传的。
//
// 建议每天跑一次。correct 为 false 时只报告不修正——第一次在生产上跑
// 应当先看一眼偏差有多大：偏差巨大往往意味着别的地方出了问题
// （比如某个租户的记录被批量误删），这时候直接把账面改成"真实值"
// 等于把事故现场也一起抹平。
func (m *DefaultManager) ReconcileUsage(ctx context.Context, namespace, accountID string, correct bool) (*UsageDrift, error) {
	if m.quota == nil {
		return nil, nil
	}
	counted, err := m.quota.GetUsage(ctx, namespace, accountID)
	if err != nil {
		return nil, err
	}
	actual, err := m.quota.ComputeUsage(ctx, namespace, accountID)
	if err != nil {
		return nil, err
	}
	drift := &UsageDrift{
		Namespace: namespace,
		AccountID: accountID,
		Counted:   counted,
		Actual:    actual,
		BytesDiff: actual.Bytes - counted.Bytes,
		FilesDiff: actual.Files - counted.Files,
	}
	if !correct || !drift.HasDrift() {
		return drift, nil
	}
	// 用增量而不是覆盖写：对账期间可能有并发上传在 $inc，
	// 直接覆盖会把那几次写入一起抹掉。加差值则与并发无关。
	if err = m.quota.AddUsage(ctx, namespace, accountID, drift.BytesDiff, drift.FilesDiff); err != nil {
		return drift, err
	}
	drift.Corrected = true
	return drift, nil
}

// ReconcileAllUsage 对账全部租户的用量。
//
// 租户清单从资源记录里现取——模块没有租户表，也不该有：
// 谁是租户是上游的事实，在这里维护一份副本迟早会和上游对不上。
// 单个租户失败不中断整批。
func (m *DefaultManager) ReconcileAllUsage(ctx context.Context, correct bool) ([]*UsageDrift, error) {
	if m.quota == nil {
		return nil, nil
	}
	tenants, err := m.listTenants(ctx)
	if err != nil {
		return nil, err
	}
	var drifts []*UsageDrift
	var errs []error
	for _, tenant := range tenants {
		drift, driftErr := m.ReconcileUsage(ctx, tenant.namespace, tenant.accountID, correct)
		if driftErr != nil {
			errs = append(errs, fmt.Errorf("reconcile usage %s/%s: %w", tenant.namespace, tenant.accountID, driftErr))
			continue
		}
		if drift.HasDrift() {
			drifts = append(drifts, drift)
		}
	}
	return drifts, errors.Join(errs...)
}

type tenantKey struct {
	namespace string
	accountID string
}

// listTenants 从资源记录里扫出有哪些租户。
//
// 分页扫全表，只在对账任务里跑。没有更省的办法——
// 要省就得维护一张租户表，而那张表与上游的真相迟早会对不上。
func (m *DefaultManager) listTenants(ctx context.Context) ([]tenantKey, error) {
	seen := make(map[tenantKey]struct{})
	var tenants []tenantKey
	var offset int64
	for {
		page, err := m.store.List(ctx, &model.Query{Offset: offset, Limit: exportPageSize})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return tenants, nil
		}
		for _, resource := range page {
			key := tenantKey{namespace: resource.Namespace, accountID: resource.AccountID}
			if _, done := seen[key]; done {
				continue
			}
			seen[key] = struct{}{}
			tenants = append(tenants, key)
		}
		if int64(len(page)) < exportPageSize {
			return tenants, nil
		}
		offset += int64(len(page))
	}
}

// idempotencyExpireAt 计算幂等键的失效时间。没有键时返回零值。
func (m *DefaultManager) idempotencyExpireAt(key string, now time.Time) time.Time {
	if key == "" {
		return time.Time{}
	}
	ttl := m.config.Manager.IdempotencyTTL
	if ttl <= 0 {
		ttl = config.DefaultIdempotencyTTL
	}
	return now.Add(time.Duration(ttl) * time.Second)
}

// ExpireIdempotencyKeys 清理已过期的幂等键。
//
// 删的是**键不是记录**：资源要一直留着，过期的只是「这次重试还能命中」
// 这个承诺。实现上是 $unset 整个字段而不是置空串——唯一稀疏索引
// 只跳过「字段不存在」的文档，置成空串会让所有过期记录因为都等于空串
// 而互相冲突。
//
// 刻意不用 Mongo 的 TTL 索引：那个会把整个文档删掉。
//
// 返回清理条数。单条失败不中断整批。
func (m *DefaultManager) ExpireIdempotencyKeys(ctx context.Context, limit int64) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	expired, err := m.store.List(ctx, &model.Query{
		IdempotencyExpiredBefore: time.Now(),
		Limit:                    limit,
	})
	if err != nil {
		return 0, err
	}
	cleared := 0
	var errs []error
	for _, resource := range expired {
		// 刻意保留原来的 UpdatedAt，不让它被推到当下。
		//
		// 历史软删除记录的保留期仍按 UpdatedAt 算（见 PurgeDeleted）。
		// 清一次键就把它刷新一遍的话，一条早就该被清理的软删记录
		// 会因为"刚被改过"而永远留在回收站里。
		patch := &model.ResourcePatch{ClearIdempotencyKey: true, UpdatedAt: resource.UpdatedAt}
		if err = m.store.Patch(ctx, resource.ID, patch); err != nil {
			errs = append(errs, fmt.Errorf("expire idempotency key of %s: %w", resource.ID, err))
			continue
		}
		cleared++
	}
	return cleared, errors.Join(errs...)
}

// SetQuotaStore 替换用量存储。传 nil 表示关闭用量维护。
// 约定在启动期调用。
func (m *DefaultManager) SetQuotaStore(qs store.QuotaStore) {
	m.quota = qs
}
