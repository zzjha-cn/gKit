package manager

import (
	"context"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

// findIdempotent 按幂等键查找本次请求是否已经处理过。
//
// 查询限定在同租户内：幂等键由调用方自己生成，不同租户撞同一个值
// 完全可能，跨租户命中就是一次越权读。
func (m *DefaultManager) findIdempotent(ctx context.Context, req *model.UploadRequest) (*model.Resource, bool, error) {
	if req.IdempotencyKey == "" {
		return nil, false, nil
	}
	found, err := m.store.List(ctx, &model.Query{
		Namespace:      req.Namespace,
		AccountID:      req.AccountID,
		IdempotencyKey: req.IdempotencyKey,
		Status:         model.ResourceStatusActive,
		Limit:          1,
	})
	if err != nil {
		return nil, false, err
	}
	if len(found) == 0 {
		return nil, false, nil
	}
	existing := found[0]
	if existing.Namespace != req.Namespace || existing.AccountID != req.AccountID {
		return nil, false, nil
	}
	if existing.Creator != req.Creator {
		return nil, false, store.ErrDuplicate
	}
	return existing, true, nil
}

// idempotentResult 把一条已存在的记录包装成上传结果。
//
// Uploaded=false 如实反映「这次没有写入任何字节」；Existed=true 与
// 去重命中共用同一个字段——对调用方而言两者是同一件事：
// 你要的东西已经在了，这是它的 ID。
// 地址重新签发而不是复用历史地址：上一条签名可能已经过期。
func (m *DefaultManager) idempotentResult(ctx context.Context, existing *model.Resource, req *model.UploadRequest) (*model.UploadResult, error) {
	url, err := m.uploadAccessURL(ctx, existing, req)
	if err != nil {
		return nil, err
	}
	m.countUpload(existing, false)
	return &model.UploadResult{Resource: m.fillURL(existing), Uploaded: false, Existed: true, URL: url}, nil
}
