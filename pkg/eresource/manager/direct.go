package manager

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

var (
	// ErrDirectUploadDisabled 未开启前端直传。
	ErrDirectUploadDisabled = errors.New("eresource: direct upload is not enabled")
	// ErrDirectUploadUnsupported 当前 provider 不支持客户端直连上传（如 local）。
	ErrDirectUploadUnsupported = errors.New("eresource: provider does not support direct upload")
	// ErrDirectUploadTicketInvalid 登记票据缺失、被改写或签名对不上。
	ErrDirectUploadTicketInvalid = errors.New("eresource: direct upload ticket is invalid")
	// ErrDirectUploadTicketExpired 登记票据已过期。
	ErrDirectUploadTicketExpired = errors.New("eresource: direct upload ticket is expired")
	// ErrDirectUploadIdentityMismatch 登记方与票据签发时的身份不一致。
	ErrDirectUploadIdentityMismatch = errors.New("eresource: direct upload ticket belongs to another caller")
	// ErrDirectUploadObjectMissing 票据有效但对象并不存在，说明直传没有真正完成。
	ErrDirectUploadObjectMissing = errors.New("eresource: direct upload object does not exist")
)

// DirectUploader 由支持前端直传的 Manager 实现。
//
// 做成可选接口而不是塞进 Manager：直传依赖对象存储，local 部署下这两个
// 方法永远只能返回错误，没必要强迫每个 Manager 实现都去实现它们。
// transport 层据此决定要不要注册直传路由。
type DirectUploader interface {
	CreateDirectUpload(ctx context.Context, req *model.DirectUploadRequest) (*model.DirectUploadPolicy, error)
	CompleteDirectUpload(ctx context.Context, req *model.DirectUploadCompleteRequest) (*model.UploadResult, error)
}

// directTicketGrace 票据相对于直传凭证额外延长的登记宽限期。
//
// 凭证过期只意味着「不能再开始上传」，一个已经开始的大文件传完可能还要很久，
// 传完之后才会回来登记。两者用同一个时限的话，大文件会稳定地传成功却登记失败，
// 在 bucket 里留下一个没有元数据的孤儿对象。
const directTicketGrace = time.Hour

// directTicket 登记票据的载荷。
//
// 这些字段必须由服务端在签发时固定下来并受 HMAC 保护：
// 它们决定了对象最终被登记进哪个租户、算谁的、用什么访问策略。
// 任何一项改由客户端在登记时上报，都等于把该维度的校验交给了攻击者。
type directTicket struct {
	ObjectKey    string `json:"k"`
	Namespace    string `json:"ns,omitempty"`
	AccountID    string `json:"acc,omitempty"`
	Creator      string `json:"cr,omitempty"`
	BizType      string `json:"bt,omitempty"`
	AccessPolicy string `json:"ap"`
	ContentType  string `json:"ct,omitempty"`
	Ext          string `json:"ext,omitempty"`
	FileName     string `json:"fn,omitempty"`
	Provider     string `json:"pv"`
	Bucket       string `json:"bk,omitempty"`
	StorePath    string `json:"sp,omitempty"`
	Staged       bool   `json:"staged,omitempty"`
	MaxSize      int64  `json:"mx"`
	ExpireAt     int64  `json:"exp"`
}

// signDirectTicket 把载荷序列化成 base64url(payload).hex(hmac) 的自包含票据。
//
// 自包含而不是在服务端存一张待确认表：直传的回调可能隔很久才来，
// 也可能永远不来，为此维护一张需要清理的中间表并不划算。
// 代价是票据一旦签出就无法撤销，因此有效期必须短。
func (m *DefaultManager) signDirectTicket(ticket *directTicket) (string, error) {
	secretKey, err := m.signedURLSecret()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(ticket)
	if err != nil {
		return "", fmt.Errorf("marshal direct upload ticket: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + directTicketSignature(payload, secretKey), nil
}

// parseDirectTicket 验签并还原票据载荷。
func (m *DefaultManager) parseDirectTicket(raw string) (*directTicket, error) {
	secretKey, err := m.signedURLSecret()
	if err != nil {
		return nil, err
	}
	payload, signature, ok := strings.Cut(strings.TrimSpace(raw), ".")
	if !ok || payload == "" || signature == "" {
		return nil, ErrDirectUploadTicketInvalid
	}
	// 恒定时间比较，与签名 URL 的验签口径保持一致。
	if !hmac.Equal([]byte(directTicketSignature(payload, secretKey)), []byte(signature)) {
		return nil, ErrDirectUploadTicketInvalid
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, ErrDirectUploadTicketInvalid
	}
	ticket := &directTicket{}
	if err = json.Unmarshal(decoded, ticket); err != nil {
		return nil, ErrDirectUploadTicketInvalid
	}
	// 先验签再判过期：过期信息只对持有合法票据的调用方暴露。
	if time.Now().Unix() > ticket.ExpireAt {
		return nil, ErrDirectUploadTicketExpired
	}
	return ticket, nil
}

func directTicketSignature(payload, secretKey string) string {
	mac := hmac.New(sha256.New, []byte(secretKey))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// CreateDirectUpload 签发前端直传凭证。
//
// 服务端在这条路径上永远看不到文件内容，因此能做的校验只有「声明值」一层：
// 扩展名白名单、MIME 白名单、public 策略的危险类型拦截都按调用方声明的
// Content-Type 判定，嗅探校验（validateMimeConsistency）整个失效。
// 这是直传的固有代价，不是实现缺陷——换来的是文件不再占用业务服务的
// 内存、带宽与上传并发槽位。真正要求内容可信的业务应继续走中转上传。
func (m *DefaultManager) CreateDirectUpload(ctx context.Context, req *model.DirectUploadRequest) (*model.DirectUploadPolicy, error) {
	if !m.config.Manager.EnableDirectUpload {
		return nil, ErrDirectUploadDisabled
	}
	signer, ok := m.provider.(provider.DirectUploader)
	if !ok {
		return nil, ErrDirectUploadUnsupported
	}
	if req == nil {
		return nil, ErrEmptyUpload
	}
	// 身份字段同样参与 object key 的路径拼装，非法值必须在签发前拒掉：
	// 凭证一旦签出，里面的 key 就是 OSS 唯一认可的落点。
	if err := provider.ValidateIdentitySegments(&model.UploadRequest{
		Namespace: req.Namespace,
		AccountID: req.AccountID,
		BizType:   req.BizType,
	}); err != nil {
		return nil, err
	}

	ext := resolveExt(req.FileName)
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	accessPolicy := m.accessPolicy(&model.UploadRequest{AccessPolicy: req.AccessPolicy})
	if err := m.validateUpload(ext, contentType); err != nil {
		return nil, err
	}
	// body 传 nil：直传签发时没有任何字节可嗅探，只能按声明值与扩展名判定。
	if err := m.validatePublicContent(accessPolicy, contentType, ext, nil); err != nil {
		return nil, err
	}

	staged := m.contentInspector() != nil
	if err := m.checkQuota(ctx, req.Namespace, req.AccountID, 0); err != nil {
		return nil, err
	}
	signReq := *req
	signReq.Ext = ext
	signReq.ContentType = contentType
	signReq.AccessPolicy = accessPolicy
	if staged {
		signReq.AccessPolicy = model.AccessPolicyPrivate
	}
	signReq.MaxSize = m.maxFileSize()
	signReq.ExpireSeconds = m.directUploadExpire()

	policy, err := signer.SignUploadPolicy(ctx, &signReq)
	if err != nil {
		return nil, err
	}

	policy.Ticket, err = m.signDirectTicket(&directTicket{
		ObjectKey:    policy.ObjectKey,
		Namespace:    req.Namespace,
		AccountID:    req.AccountID,
		Creator:      req.Creator,
		BizType:      req.BizType,
		AccessPolicy: accessPolicy,
		ContentType:  contentType,
		Ext:          ext,
		FileName:     req.FileName,
		Provider:     policy.Provider,
		Bucket:       policy.Bucket,
		StorePath:    policy.StorePath,
		MaxSize:      policy.MaxSize,
		Staged:       staged,
		ExpireAt:     policy.ExpireAt.Add(directTicketGrace).Unix(),
	})
	if err != nil {
		return nil, err
	}
	return policy, nil
}

// directUploadExpire 返回生效的直传凭证有效期。
func (m *DefaultManager) directUploadExpire() int64 {
	if m.config.Manager.DirectUploadExpire > 0 {
		return m.config.Manager.DirectUploadExpire
	}
	return DefaultDirectUploadExpire
}

// DefaultDirectUploadExpire 未配置 DirectUploadExpire 时的兜底有效期。
const DefaultDirectUploadExpire int64 = 900

// CompleteDirectUpload 在客户端直传完成后登记元数据。
//
// 这一步必须回到服务端：对象躺在 bucket 里而 store 里没有记录的话，
// 它既查不到也不会被任何清理逻辑回收，等同于一份永久付费的垃圾。
//
// 对象是否真的传上去了以 provider.Stat 为准，不采信客户端上报：
// 否则调用方可以只拿凭证不上传，直接登记出一条指向空对象的资源记录。
func (m *DefaultManager) CompleteDirectUpload(ctx context.Context, req *model.DirectUploadCompleteRequest) (*model.UploadResult, error) {
	if !m.config.Manager.EnableDirectUpload {
		return nil, ErrDirectUploadDisabled
	}
	if req == nil {
		return nil, ErrDirectUploadTicketInvalid
	}
	// metadata 的 key 在碰任何存储之前就校验：登记失败是可以重试的，
	// 但如果等到 Stat 之后才拒绝，客户端很容易以为「传上去了只是登记失败」
	// 而反复重试同一份非法参数。
	if err := validateMetadataMap(req.Metadata); err != nil {
		return nil, err
	}
	ticket, err := m.parseDirectTicket(req.Ticket)
	if err != nil {
		return nil, err
	}
	// 票据是不记名字符串，泄露出去就是一次可用的写入许可。
	// 再比对一次调用方身份，把「谁持有谁能用」收窄成「签给谁才能用」。
	if ticket.Namespace != req.Namespace || ticket.AccountID != req.AccountID || ticket.Creator != req.Creator {
		return nil, ErrDirectUploadIdentityMismatch
	}

	id := completedUploadID("direct", ticket.ObjectKey)
	if result, err := m.completedUpload(ctx, id, ticket.Creator, req.ExpireSeconds); result != nil || err != nil {
		return result, err
	}
	if result, err := m.previousUpload(ctx, ticket.ObjectKey, ticket.Namespace, ticket.AccountID, ticket.Creator, req.ExpireSeconds); result != nil || err != nil {
		return result, err
	}
	stat, err := m.provider.Stat(ctx, ticket.ObjectKey)
	if err != nil {
		return nil, err
	}
	if stat == nil || !stat.Exists {
		if result, err := m.completedUpload(ctx, id, ticket.Creator, req.ExpireSeconds); result != nil || err != nil {
			return result, err
		}
		return nil, fmt.Errorf("%w: object=%q", ErrDirectUploadObjectMissing, ticket.ObjectKey)
	}
	// OSS 侧的 content-length-range 已经拦过一道，这里再核一次：
	// 两道校验基于不同的信任源，配置改动或条件写漏时还有兜底。
	if stat.Size > ticket.MaxSize {
		_ = m.provider.Delete(ctx, ticket.ObjectKey)
		return nil, fmt.Errorf("%w: limit=%d", ErrFileTooLarge, ticket.MaxSize)
	}

	if err = m.checkQuota(ctx, ticket.Namespace, ticket.AccountID, stat.Size); err != nil {
		return nil, err
	}
	mimeType := ticket.ContentType
	if mimeType == "" {
		mimeType = stat.ContentType
	}
	fileName := req.FileName
	if fileName == "" {
		fileName = ticket.FileName
	}

	now := time.Now()
	resource := &model.Resource{
		ID:        id,
		Namespace: ticket.Namespace,
		AccountID: ticket.AccountID,
		Name:      fileName,
		Ext:       ticket.Ext,
		MimeType:  mimeType,
		Size:      stat.Size,
		// Hash 留空：服务端没读过内容，算不出 MD5。
		// 直传资源因此不参与去重——宁可多存一份，也不能用客户端上报的
		// 摘要去匹配别人的对象（那等于拿一个可伪造的值做身份断言）。
		Provider:  ticket.Provider,
		Bucket:    ticket.Bucket,
		ObjectKey: ticket.ObjectKey,
		StorePath: ticket.StorePath,

		Scheme:            m.config.Provider.Scheme,
		Status:            model.ResourceStatusActive,
		BizType:           ticket.BizType,
		Tags:              normalizeTags(req.Tags),
		AccessPolicy:      ticket.AccessPolicy,
		HotlinkProtection: m.config.Access.EnableHotlinkProtection,
		Creator:           ticket.Creator,
		Metadata:          req.Metadata,
		Version:           1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if ticket.Staged || m.contentInspector() != nil {
		if !ticket.Staged && ticket.AccessPolicy == model.AccessPolicyPublic {
			return nil, ErrPublicUploadRequiresInspection
		}
		body, inspectErr := m.readAndInspectObject(ctx, resource, ticket.MaxSize)
		if inspectErr != nil {
			return nil, inspectErr
		}
		// Publish a copy of the inspected bytes at a key the client has no write grant for.
		published, err := m.provider.Upload(ctx, &model.UploadRequest{
			Namespace: resource.Namespace, AccountID: resource.AccountID, Creator: resource.Creator,
			BizType: resource.BizType, FileName: resource.Name, Ext: resource.Ext,
			Bytes: body, Hash: md5Hash(body), ContentType: resource.MimeType, AccessPolicy: resource.AccessPolicy,
		})
		if err != nil {
			return nil, err
		}
		resource.Provider = published.Provider
		resource.Bucket = published.Bucket
		resource.ObjectKey = published.ObjectKey
		resource.StorePath = published.StorePath
	}
	resultURL, err := m.uploadAccessURL(ctx, resource, &model.UploadRequest{
		Creator:       ticket.Creator,
		ExpireSeconds: req.ExpireSeconds,
	})
	if err != nil {
		if resource.ObjectKey != ticket.ObjectKey {
			_ = m.provider.Delete(ctx, resource.ObjectKey)
		}
		return nil, err
	}
	// Keep the source object on registration failure so the client can retry.
	if err = m.store.Create(ctx, resource); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			if result, lookupErr := m.completedUpload(ctx, id, ticket.Creator, req.ExpireSeconds); result != nil || lookupErr != nil {
				if result != nil && resource.ObjectKey != ticket.ObjectKey && resource.ObjectKey != result.Resource.ObjectKey {
					_ = m.provider.Delete(ctx, resource.ObjectKey)
				}
				return result, lookupErr
			}
		}
		return nil, err
	}
	if resource.ObjectKey != ticket.ObjectKey {
		_ = m.provider.Delete(ctx, ticket.ObjectKey)
	}
	m.addUsage(ctx, resource.Namespace, resource.AccountID, resource.Size, 1)
	return &model.UploadResult{Resource: m.fillURL(resource), Uploaded: true, URL: resultURL}, nil
}

// completedUploadID gives every session a stable resource ID across service instances.
func completedUploadID(kind, key string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + key))
	return hex.EncodeToString(digest[:])
}

func (m *DefaultManager) completedUpload(ctx context.Context, id, creator string, expireSeconds int64) (*model.UploadResult, error) {
	resource, err := m.store.GetByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = m.authorizeIdentity(resource, nil); err != nil {
		return nil, err
	}
	return m.idempotentResult(ctx, resource, &model.UploadRequest{Creator: creator, ExpireSeconds: expireSeconds})
}

// previousUpload recognizes resources completed before stable upload IDs were introduced.
func (m *DefaultManager) previousUpload(ctx context.Context, key, namespace, accountID, creator string, expireSeconds int64) (*model.UploadResult, error) {
	resources, err := m.store.List(ctx, &model.Query{Provider: m.provider.Name(), ObjectKey: key, Limit: 1})
	if err != nil || len(resources) == 0 {
		return nil, err
	}
	resource := resources[0]
	if resource.Namespace != namespace || resource.AccountID != accountID || resource.Creator != creator {
		return nil, ErrResourceNotVisible
	}
	if err := m.authorizeIdentity(resource, nil); err != nil {
		return nil, err
	}
	return m.idempotentResult(ctx, resource, &model.UploadRequest{Creator: creator, ExpireSeconds: expireSeconds})
}
