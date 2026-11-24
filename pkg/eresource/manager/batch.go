package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

// MaxBatchSize 单次批量操作允许的 ID 数量上限。
//
// 必须有上限：批量接口的代价随 ID 数线性增长（签名是逐条算的，
// 批量删除还要逐条访问底层存储），没有上限时一个请求就能把一个 worker 占满。
const MaxBatchSize = model.MaxQueryIDs

var (
	// ErrEmptyBatch 批量请求没带任何 ID。
	ErrEmptyBatch = errors.New("eresource: batch request contains no id")
	// ErrBatchTooLarge 批量请求的 ID 数超过上限。
	ErrBatchTooLarge = fmt.Errorf("eresource: batch request exceeds %d ids", MaxBatchSize)
)

// BatchResult 批量操作中单条资源的处理结果。
//
// Err 非 nil 表示这一条失败了，整批仍然继续——批量接口的调用方拿着
// 20 个 ID 来签名，其中一个被删了不该让另外 19 个也拿不到地址。
type BatchResult struct {
	ID  string
	URL string
	Err error
}

// BatchAccessURL 批量签发访问地址。
//
// 一个 20 张图的列表页在没有这个接口时要发 20 次请求、查 20 次库。
// 这里只查一次：按 ID 集合一次性捞出元数据，再逐条做鉴权与签名。
// 鉴权本身无法合并——每条资源的访问策略、归属与频控都是独立的。
func (m *DefaultManager) BatchAccessURL(ctx context.Context, ids []string, req *model.AccessRequest) ([]BatchResult, error) {
	ids, err := normalizeBatchIDs(ids)
	if err != nil {
		return nil, err
	}
	// 用调用方身份强制收窄检索范围，与 List 同一套隔离方式：
	// 别的租户的 ID 混在请求里时，它们不会出现在结果里，表现为「不存在」。
	query := ScopedQuery(&model.Query{IDs: ids, Limit: int64(len(ids))}, req)
	found, err := m.store.List(ctx, query)
	if err != nil {
		return nil, err
	}
	indexed := make(map[string]*model.Resource, len(found))
	for _, resource := range found {
		indexed[resource.ID] = resource
	}
	userID := ""
	if req != nil {
		userID = req.UserID
	}
	results := make([]BatchResult, 0, len(ids))
	for _, id := range ids {
		resource, ok := indexed[id]
		if !ok {
			results = append(results, BatchResult{ID: id, Err: store.ErrNotFound})
			continue
		}
		if err = m.verifyResourceAccess(ctx, resource, req); err != nil {
			results = append(results, BatchResult{ID: id, Err: err})
			continue
		}
		url, urlErr := m.accessURLFor(ctx, resource, userID)
		if urlErr != nil {
			results = append(results, BatchResult{ID: id, Err: urlErr})
			continue
		}
		results = append(results, BatchResult{ID: id, URL: url})
	}
	return results, nil
}

// BatchDelete 批量删除资源。
//
// 刻意逐条走 Delete 而不是批量操作存储：每条都要做归属校验、都要触碰
// 底层对象、都要数引用，把这些合并成一条语句得不到什么，却会让批量删除与
// 单条删除的语义慢慢长成两套。
func (m *DefaultManager) BatchDelete(ctx context.Context, ids []string, req *model.AccessRequest) ([]BatchResult, error) {
	ids, err := normalizeBatchIDs(ids)
	if err != nil {
		return nil, err
	}
	results := make([]BatchResult, 0, len(ids))
	for _, id := range ids {
		results = append(results, BatchResult{ID: id, Err: m.Delete(ctx, id, req)})
	}
	return results, nil
}

// normalizeBatchIDs 去空、去重并校验批量规模，保持调用方给出的顺序。
//
// 去重是必要的：同一个 ID 传十次会被签十次名，也会被删十次——
// 后九次拿到的是「不存在」，对调用方而言是一批莫名其妙的失败项。
func normalizeBatchIDs(ids []string) ([]string, error) {
	normalized := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	if len(normalized) == 0 {
		return nil, ErrEmptyBatch
	}
	if len(normalized) > MaxBatchSize {
		return nil, ErrBatchTooLarge
	}
	return normalized, nil
}
