package manager

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/rs/xid"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

func (m *DefaultManager) tryDedup(ctx context.Context, req *model.UploadRequest, hash, ext, mimeType, accessPolicy string) (*model.UploadResult, error) {
	m.objectReferencesMu.Lock()
	defer m.objectReferencesMu.Unlock()
	existing, err := m.findDedupTarget(ctx, req, hash, accessPolicy)
	if err != nil || existing == nil {
		return nil, err
	}
	return m.onDedupHit(ctx, existing, req, ext, mimeType, accessPolicy)
}

// findDedupTarget 查找可复用的历史资源。
//
// 去重范围必须限定在同租户 / 同 provider / 同业务 / 同访问策略内，否则
// A 用户上传的 public 图片会被 B 用户的 private 上传直接复用，等于越权泄露。
// Namespace 与 AccountID 是租户隔离边界：少了这两个条件，
// 命中的会是别的租户的 Resource（连同它的 ID、Creator、Metadata 一起返回）。
// 非 public 资源还要求同一上传人，避免跨用户串号。
func (m *DefaultManager) findDedupTarget(ctx context.Context, req *model.UploadRequest, hash, accessPolicy string) (*model.Resource, error) {
	query := &model.Query{
		Namespace:    req.Namespace,
		AccountID:    req.AccountID,
		Hash:         hash,
		Provider:     m.provider.Name(),
		BizType:      req.BizType,
		Status:       model.ResourceStatusActive,
		AccessPolicy: accessPolicy,
		Limit:        1,
	}
	if accessPolicy != model.AccessPolicyPublic {
		query.Creator = req.Creator
	}
	found, err := m.store.List(ctx, query)
	if err != nil {
		// 存储不可用时直接失败，不要退化成静默重新上传而掩盖故障
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	// Store 的 Query 把空串当作「不过滤该字段」，所以调用方没传租户信息时，
	// 上面的条件并不足以把结果限制在本租户内。这里再显式核对一次边界：
	// 宁可放弃一次去重（多存一份副本），也不能把别的租户的资源认成自己的。
	if found[0].Namespace != req.Namespace || found[0].AccountID != req.AccountID {
		return nil, nil
	}
	// 底层对象真的还在，这次秒传才成立。
	// 对象被外部手工清理过（或补偿删除删了一半）时，元数据还留着，
	// 不核一下就会返回一条指向 404 的记录——而调用方刚刚明明上传成功了。
	// 查不到就当没命中，重新走一次完整上传，代价只是多占一份空间。
	stat, err := m.provider.Stat(ctx, found[0].ObjectKey)
	if err != nil {
		// 与上面的 List 同一个口径：存储不可用时直接失败，
		// 不要退化成静默重新上传而掩盖故障。
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if stat == nil || !stat.Exists {
		return nil, nil
	}
	return found[0], nil
}

// onDedupHit 处理一次去重命中，按配置的策略决定复用方式。
func (m *DefaultManager) onDedupHit(ctx context.Context, existing *model.Resource,
	req *model.UploadRequest, ext, mimeType, accessPolicy string) (*model.UploadResult, error) {
	m.metricsSink().Count(MetricDedupHit, 1, map[string]string{
		"namespace": req.Namespace,
		"strategy":  m.config.Manager.DedupStrategy,
	})
	if m.config.Manager.DedupStrategy == config.DedupStrategyReuseRecord && req.IdempotencyKey == "" {
		url, err := m.accessURLFor(ctx, existing, existing.Creator)
		if err != nil {
			return nil, err
		}
		return &model.UploadResult{Resource: existing, Uploaded: false, Existed: true, URL: url}, nil
	}
	return m.registerDedupCopy(ctx, existing, req, ext, mimeType, accessPolicy)
}

// registerDedupCopy 复用已有对象，但为本次上传新建一条记录。
//
// 这是 new_record 策略：省掉的是一次底层写入（秒传真正的价值），
// 保留的是「这次上传有它自己的 ID、文件名、标签与 metadata」。
// reuse_record 直接返回历史记录，调用方拿着那个 ID 去关联本次业务对象时，
// 关到的是别人几个月前上传的那一条。
//
// 这里绝对不能在失败时删除底层对象：它不是本次写入的，上面还挂着
// 至少一条别人的记录。普通上传路径的回滚删除在这条路径上必须缺席。
func (m *DefaultManager) registerDedupCopy(ctx context.Context, existing *model.Resource,
	req *model.UploadRequest, ext, mimeType, accessPolicy string) (*model.UploadResult, error) {
	now := time.Now()
	resource := &model.Resource{
		ID:        xid.New().String(),
		Namespace: req.Namespace,
		AccountID: req.AccountID,
		Name:      req.FileName,
		Ext:       ext,
		MimeType:  mimeType,
		// 内容事实全部取自被复用的那条记录：同一个对象，大小与哈希必然相同。
		Size:      existing.Size,
		Hash:      existing.Hash,
		Provider:  existing.Provider,
		Bucket:    existing.Bucket,
		ObjectKey: existing.ObjectKey,
		StorePath: existing.StorePath,

		Scheme: m.config.Provider.Scheme,
		Status: model.ResourceStatusActive,
		// 业务字段全部取自本次请求，这正是 new_record 存在的理由。
		BizType:             req.BizType,
		Tags:                normalizeTags(req.Tags),
		AccessPolicy:        accessPolicy,
		HotlinkProtection:   m.config.Access.EnableHotlinkProtection,
		Creator:             req.Creator,
		Metadata:            req.Metadata,
		IdempotencyKey:      req.IdempotencyKey,
		IdempotencyExpireAt: m.idempotencyExpireAt(req.IdempotencyKey, now),
		Shared:              req.Shared && req.IsAdmin,
		Version:             1,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	var err error
	if accessPolicy == model.AccessPolicyPublic {
		if resource.URL, err = m.publicURL(resource.ObjectKey); err != nil {
			return nil, err
		}
	}
	resultURL, err := m.uploadAccessURL(ctx, resource, req)
	if err != nil {
		return nil, err
	}
	if err = m.store.Create(ctx, resource); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			if winner, hit, lookupErr := m.findIdempotent(ctx, req); lookupErr == nil && hit {
				return m.idempotentResult(ctx, winner, req)
			}
		}
		return nil, err
	}
	return &model.UploadResult{Resource: resource, Uploaded: false, Existed: true, URL: resultURL}, nil
}
