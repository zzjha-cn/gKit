package manager

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"time"

	"github.com/rs/xid"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

func (m *DefaultManager) Upload(ctx context.Context, req *model.UploadRequest) (*model.UploadResult, error) {
	// 先校验身份字段：非法的 namespace / account_id / biz_type 不值得为它读完整个请求体。
	if err := provider.ValidateIdentitySegments(req); err != nil {
		return nil, err
	}
	body, err := m.readUploadBody(req)
	if err != nil {
		return nil, err
	}
	ext := resolveExt(req.FileName)
	mimeType := resolveMimeType(req.ContentType, body)
	if err = m.validateUpload(ext, mimeType); err != nil {
		return nil, err
	}
	if err = m.validateMimeConsistency(req.ContentType, body); err != nil {
		return nil, err
	}
	accessPolicy := m.accessPolicy(req)
	if err = m.validatePublicContent(accessPolicy, req.ContentType, ext, body); err != nil {
		return nil, err
	}
	if err = validateMetadataMap(req.Metadata); err != nil {
		return nil, err
	}
	// 幂等键先于一切存储动作检查：命中的话这次请求根本不该碰底层对象。
	if existing, hit, idemErr := m.findIdempotent(ctx, req); idemErr != nil {
		return nil, idemErr
	} else if hit {
		return m.idempotentResult(ctx, existing, req)
	}
	if err = m.checkQuota(ctx, req.Namespace, req.AccountID, int64(len(body))); err != nil {
		return nil, err
	}
	hash := md5Hash(body)
	if m.config.Manager.EnableDedup && m.contentInspector() == nil {
		returnResult, getErr := m.tryDedup(ctx, req, hash, ext, mimeType, accessPolicy)
		if getErr != nil {
			return nil, getErr
		}
		if returnResult != nil {
			return returnResult, nil
		}
	}
	uploadReq := *req
	uploadReq.Reader = nil
	uploadReq.Bytes = body
	uploadReq.Hash = hash
	uploadReq.Ext = ext
	// 回填解析后的访问策略：调用方可以不传，由配置兜底，
	// 但 Provider 需要据此决定对象 ACL（OSS），不能让它看到空串再自己猜。
	uploadReq.AccessPolicy = accessPolicy
	now := time.Now()
	resource := &model.Resource{
		ID:        xid.New().String(),
		Namespace: req.Namespace,
		AccountID: req.AccountID,
		Name:      req.FileName,
		Ext:       ext,
		MimeType:  mimeType,
		Size:      int64(len(body)),
		Hash:      hash,

		Scheme:            m.config.Provider.Scheme,
		Status:            model.ResourceStatusActive,
		BizType:           req.BizType,
		Tags:              normalizeTags(req.Tags),
		AccessPolicy:      accessPolicy,
		HotlinkProtection: m.config.Access.EnableHotlinkProtection,
		Creator:           req.Creator,
		Metadata:          req.Metadata,
		IdempotencyKey:    req.IdempotencyKey,
		// 幂等键自带有效期：过期之后由后台任务把键摘掉，
		// 记录本身保留。不设上限的话那条唯一稀疏索引只增不减。
		IdempotencyExpireAt: m.idempotencyExpireAt(req.IdempotencyKey, now),
		// Shared 只认管理员：普通业务调用设了这个字段会被静默丢弃，
		// 否则任何租户都能把自己的资源变成全局可读。
		Shared: req.Shared && req.IsAdmin,
		// 版本从 1 起算。当前不实现版本管理，字段只是占位，
		// 但留一个确定的起点，将来接版本链时不必回填存量数据。
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err = m.inspectContent(ctx, resource, body); err != nil {
		return nil, err
	}
	providerResult, err := m.provider.Upload(ctx, &uploadReq)
	if err != nil {
		return nil, err
	}
	resource.Size = providerResult.Size
	resource.Provider = providerResult.Provider
	resource.Bucket = providerResult.Bucket
	resource.ObjectKey = providerResult.ObjectKey
	resource.StorePath = providerResult.StorePath
	// public 的直链算出来只是为了回给本次调用，**不落库**。
	// 库里存一份上传当时的域名快照，换 CDN 域名就得刷库（B13）；
	// 读的时候由 fillURL 按当时的配置重算。
	publicURL := ""
	if accessPolicy == model.AccessPolicyPublic {
		if publicURL, err = m.publicURLFor(providerResult.ObjectKey, resource.AccountID); err != nil {
			_ = m.provider.Delete(ctx, providerResult.ObjectKey)
			return nil, err
		}
	}

	resultURL, err := m.uploadAccessURL(ctx, resource, req)
	if err != nil {
		_ = m.provider.Delete(ctx, providerResult.ObjectKey)
		return nil, err
	}

	if err = m.store.Create(ctx, resource); err != nil {
		// 对象已经写进去了，这次却没能登记——无论如何都要把它删掉，
		// 否则留下的是一个谁都找不到、却一直在计费的孤儿。
		_ = m.provider.Delete(ctx, providerResult.ObjectKey)
		// 幂等键撞车说明有一个并发的同键请求抢先登记成功了。
		// 这不是失败：回查它那一条返回，两个请求看到同一个结果。
		if errors.Is(err, store.ErrDuplicate) {
			if winner, hit, lookupErr := m.findIdempotent(ctx, req); lookupErr == nil && hit {
				return m.idempotentResult(ctx, winner, req)
			}
		}
		return nil, err
	}
	m.addUsage(ctx, resource.Namespace, resource.AccountID, resource.Size, 1)
	m.countUpload(resource, true)
	resource.URL = publicURL
	return &model.UploadResult{Resource: resource, Uploaded: true, URL: resultURL}, nil
}

func (m *DefaultManager) accessPolicy(req *model.UploadRequest) string {
	if req.AccessPolicy != "" {
		return req.AccessPolicy
	}
	if m.config.Manager.DefaultAccessPolicy != "" {
		return m.config.Manager.DefaultAccessPolicy
	}
	return model.AccessPolicyPrivate
}

func md5Hash(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
