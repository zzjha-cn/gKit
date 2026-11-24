package manager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// ErrListUnsupported 当前 provider 不支持列举对象，无法对账。
var ErrListUnsupported = errors.New("eresource: provider does not support listing objects")

// ReconcileResult 一次对账的结果。
type ReconcileResult struct {
	Scanned int      // 扫过的对象数
	Orphans int      // 判定为孤儿的对象数
	Deleted int      // 实际删除的对象数（DryRun 时恒为 0）
	Missing []string // 库里有记录、底层却不存在的 object key
	Next    string   // 非空表示还有下一页，下次把它传回 marker
}

// ReconcileOptions 对账参数。
type ReconcileOptions struct {
	// Prefix 限定对账范围，典型用法是一次只扫一个租户。
	Prefix string
	// Marker 分页游标，传上一次返回的 Next。
	Marker string
	// Limit 单批扫描的对象数。
	Limit int
	// Before 安全窗口：只有最后修改时间早于该时刻的对象才可能被判成孤儿。
	//
	// 这**不是**优化，是正确性的一部分。上传是「先写对象、后建记录」
	// 两步，正在进行中的上传在这两步之间看起来和孤儿一模一样。
	// 窗口必须大于最长一次上传耗时，而在开了前端直传的部署里，
	// 它还必须大于 manager.direct_upload_expire——那条路径上对象先落、
	// 客户端稍后才调 /complete 建记录，中间可以隔很久。
	//
	// 零值表示「不设窗口」，ReconcileOrphans 会直接拒绝执行：
	// 这个参数漏传的后果是删掉正在上传的文件，不能有默认值兜底。
	Before time.Time
	// DryRun 只报告不删除。第一次在生产上跑务必先用它看一眼结果。
	DryRun bool
}

// ErrUnsafeReconcileWindow 对账缺少安全窗口。
var ErrUnsafeReconcileWindow = errors.New("eresource: reconcile requires a non-zero Before window")

// ReconcileOrphans 对账一批底层对象，清理没有任何记录指向的孤儿。
//
// 为什么需要它：上传路径是「provider.Upload 成功 → store.Create」两步，
// 进程在中间崩溃就留下一个谁也查不到、却一直在计费的对象。
// 现有的补偿删除只覆盖「Create 返回了错误且进程还活着」这一种，
// 崩溃路径没有任何清理，成本随时间单调增长。
//
// 刻意做成由调用方驱动的分批接口而不是内置后台 goroutine：
// 清理动作要可观测、可暂停、可按租户分批，内置定时器三样都做不到。
func (m *DefaultManager) ReconcileOrphans(ctx context.Context, opts *ReconcileOptions) (*ReconcileResult, error) {
	if opts == nil {
		return nil, ErrUnsafeReconcileWindow
	}
	if opts.Before.IsZero() {
		return nil, ErrUnsafeReconcileWindow
	}
	lister, ok := m.provider.(provider.ObjectLister)
	if !ok {
		return nil, ErrListUnsupported
	}
	entries, next, err := lister.ListObjects(ctx, opts.Prefix, opts.Marker, opts.Limit)
	if err != nil {
		return nil, err
	}
	result := &ReconcileResult{Scanned: len(entries), Next: next}
	var errs []error
	for _, entry := range entries {
		// 分片暂存区不参与对账：那里的文件本来就没有资源记录指向，
		// 在对账眼里和孤儿一模一样，而它们恰恰是一次正在进行的上传。
		// 清理它们是 AbortMultipart 与分片会话过期的职责。
		if provider.IsMultipartStaging(entry.Key) {
			continue
		}
		// 窗口内的对象一律跳过：它可能是一次正在进行的上传。
		if !entry.UpdatedAt.Before(opts.Before) {
			continue
		}
		referenced, countErr := m.objectIsReferenced(ctx, entry.Key)
		if countErr != nil {
			errs = append(errs, fmt.Errorf("count references for %s: %w", entry.Key, countErr))
			continue
		}
		if referenced {
			continue
		}
		result.Orphans++
		if opts.DryRun {
			continue
		}
		if delErr := m.provider.Delete(ctx, entry.Key); delErr != nil && !errors.Is(delErr, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("delete orphan %s: %w", entry.Key, delErr))
			continue
		}
		result.Deleted++
	}
	return result, errors.Join(errs...)
}

// objectIsReferenced 判断底层对象是否还有记录指向。
//
// 包含已软删的记录：它们的对象还在保留期里，删掉就把「可恢复」这件事
// 变成了谎言。这里不带 Status 条件正是为此。
func (m *DefaultManager) objectIsReferenced(ctx context.Context, objectKey string) (bool, error) {
	count, err := m.store.Count(ctx, &model.Query{
		Provider:  m.provider.Name(),
		ObjectKey: objectKey,
	})
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ReconcileMissing 反向对账：找出库里有记录、底层却已经不存在的资源。
//
// 处理方式与孤儿相反——**只报告，不删记录**。这种不一致要么是外部误删，
// 要么是模块自己的 bug，两种情况下那条记录都是现场唯一的证据，
// 自动清掉等于把线索销毁。清不清、怎么清，交给看到报告的人决定。
func (m *DefaultManager) ReconcileMissing(ctx context.Context, query *model.Query) ([]string, error) {
	if query == nil {
		query = &model.Query{}
	}
	resources, err := m.store.List(ctx, query)
	if err != nil {
		return nil, err
	}
	var missing []string
	var errs []error
	for _, resource := range resources {
		stat, statErr := m.provider.Stat(ctx, resource.ObjectKey)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				missing = append(missing, resource.ID)
				continue
			}
			errs = append(errs, fmt.Errorf("stat %s: %w", resource.ObjectKey, statErr))
			continue
		}
		if stat == nil || !stat.Exists {
			missing = append(missing, resource.ID)
		}
	}
	return missing, errors.Join(errs...)
}
