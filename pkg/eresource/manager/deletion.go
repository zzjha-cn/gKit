package manager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

// Delete 删除资源元数据及其底层对象。
//
// req 承载调用方身份，用于租户与归属校验；传 nil 表示可信的进程内调用。
func (m *DefaultManager) Delete(ctx context.Context, id string, req *model.AccessRequest) error {
	resource, err := m.store.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err = m.authorizeScope(resource, req); err != nil {
		return err
	}
	if err = m.authorizeMutate(resource, req); err != nil {
		return err
	}
	if m.config.Access.SoftDelete {
		return m.markDeleted(ctx, resource)
	}
	return m.purge(ctx, resource)
}

// markDeleted 只翻状态，保留元数据与底层对象。
// 状态一旦不是 active，authorizeIdentity 就会挡住全部读取路径，
// 对调用方而言与删除无异，但还留着一个可恢复的窗口。
func (m *DefaultManager) markDeleted(ctx context.Context, resource *model.Resource) error {
	m.objectReferencesMu.Lock()
	defer m.objectReferencesMu.Unlock()
	current, err := m.store.GetByID(ctx, resource.ID)
	if err != nil {
		return err
	}
	resource = current
	if resource.Status == model.ResourceStatusDeleted {
		return nil
	}
	if resource.AccessPolicy == model.AccessPolicyPublic {
		count, err := m.store.Count(ctx, &model.Query{Provider: resource.Provider, ObjectKey: resource.ObjectKey, Status: model.ResourceStatusActive})
		if err != nil {
			return err
		}
		if count <= 1 {
			if err := m.applyObjectACL(ctx, resource.ObjectKey, model.AccessPolicyPrivate); err != nil {
				return err
			}
			if refresher := m.cdnRefresher(); refresher != nil {
				publicURL, err := m.publicURLFor(resource.ObjectKey, resource.AccountID)
				if err != nil {
					return err
				}
				if err = refresher.Refresh(ctx, []string{publicURL}); err != nil {
					return err
				}
			}
		}
	}
	deleted := model.ResourceStatusDeleted
	now := time.Now()
	return m.store.Patch(ctx, resource.ID, &model.ResourcePatch{
		Status:    &deleted,
		DeletedAt: &now,
		UpdatedAt: now,
	})
}

// purge 真正抹掉元数据，并在没有别的记录引用时删掉底层对象，不可撤销。
//
// 为什么要先数引用：开了去重的 new_record 策略之后，多条记录会指向同一个
// 对象，「记录 : 对象」从 1:1 变成了 N:1。无条件删对象的话，删掉其中一条
// 记录就会让所有兄弟记录一起变成 404——一次合法的删除操作，损坏的是别人的数据。
//
// 竞态是存在的：两条记录同时被删，双方都看到对方还在、于是都不删对象，
// 留下一个没有记录指向的孤儿对象。这个方向是刻意选的——孤儿对象只浪费空间，
// 可以靠对账清理；反过来误删对象则是不可逆的数据丢失。
func (m *DefaultManager) purge(ctx context.Context, resource *model.Resource) error {
	m.objectReferencesMu.Lock()
	defer m.objectReferencesMu.Unlock()
	current, err := m.store.GetByID(ctx, resource.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	resource = current
	references, err := m.objectReferences(ctx, resource)
	if err != nil {
		return err
	}
	// references <= 1 表示只有自己在引用（0 出现在 store 不支持按 object key
	// 检索或 object key 为空的边界情况上，按独占处理与旧行为一致）。
	objectRemoved := false
	if references <= 1 {
		// 底层对象已不存在时继续删除元数据，否则这条记录会永远删不掉。
		if err = m.provider.Delete(ctx, resource.ObjectKey); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		objectRemoved = true
	}
	if err = m.store.Delete(ctx, resource.ID); err != nil {
		return err
	}
	// 用量跟着**对象**走，不跟着记录走：N 条记录共享一个对象时，
	// 删掉其中一条并没有释放任何字节。只有真的把对象删了才扣减。
	if objectRemoved {
		m.addUsage(ctx, resource.Namespace, resource.AccountID, -resource.Size, -1)
	}
	return nil
}

// PurgeDeleted 清理在 before 之前被软删除的资源，返回实际清理的条数。
//
// 软删除只是把回收窗口做出来，窗口之外的对象必须真删——否则存储成本
// 会随时间单调上涨。调用方自行调度（定时任务 / 运维脚本），
// 模块不内置后台 goroutine：清理动作应当可观测、可暂停、可按租户分批。
//
// limit 限制单批处理条数（<=0 时取 100），方便分批跑而不是一次扫全表。
// 单条失败不中断整批：一个对象删不掉不该把后面的记录一起卡住。
// ErrNoRetentionWindow 没有配置保留期，无法按保留期清理。
var ErrNoRetentionWindow = errors.New("eresource: access.purge_retention_days is not configured")

// PurgeExpired 按配置的保留期清理已软删的资源。
//
// 相比直接调 PurgeDeleted，它的价值是把「保留期多长」这件事收敛到配置里：
// 由每个调度脚本自己算 before，迟早会出现一个脚本传了 24 小时、
// 另一个传了 30 天，而它们清的是同一批数据。
func (m *DefaultManager) PurgeExpired(ctx context.Context, limit int64) (int, error) {
	days := m.config.Access.PurgeRetentionDays
	if days <= 0 {
		return 0, ErrNoRetentionWindow
	}
	return m.PurgeDeleted(ctx, time.Now().AddDate(0, 0, -days), limit)
}

func (m *DefaultManager) PurgeDeleted(ctx context.Context, before time.Time, limit int64) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	candidates, err := m.store.List(ctx, &model.Query{
		Status:        model.ResourceStatusDeleted,
		DeletedBefore: before,
		Limit:         limit,
	})
	if err != nil {
		return 0, err
	}
	purged := 0
	var errs []error
	for _, resource := range candidates {
		if err = m.purge(ctx, resource); err != nil {
			errs = append(errs, fmt.Errorf("purge %s: %w", resource.ID, err))
			continue
		}
		purged++
	}
	return purged, errors.Join(errs...)
}
