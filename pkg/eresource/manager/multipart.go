package manager

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/rs/xid"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

var (
	// ErrMultipartDisabled 分片上传未启用。
	ErrMultipartDisabled = errors.New("eresource: multipart upload is disabled")
	// ErrMultipartTicketInvalid 分片会话票据非法。
	ErrMultipartTicketInvalid = errors.New("eresource: multipart ticket is invalid")
	// ErrMultipartTicketExpired 分片会话已过期。
	ErrMultipartTicketExpired = errors.New("eresource: multipart ticket is expired")
	// ErrNoParts 完成请求里没有任何分片。
	ErrNoParts = errors.New("eresource: multipart complete requires at least one part")
	// ErrPartTooSmall 非末片小于对象存储要求的下限。
	ErrPartTooSmall = errors.New("eresource: non-final part is smaller than the minimum part size")
)

// multipartTicket 自包含的分片会话。
//
// 与直传票据同一套理由：分片上传可能持续几十分钟，也可能传到一半
// 客户端再也不回来。为此维护一张需要定期清理的会话表并不划算，
// 票据自带身份与过期时间，服务端无状态。
type multipartTicket struct {
	ObjectKey      string `json:"ok"`
	UploadID       string `json:"uid"`
	Namespace      string `json:"ns,omitempty"`
	AccountID      string `json:"acc,omitempty"`
	Creator        string `json:"cr,omitempty"`
	BizType        string `json:"bt,omitempty"`
	AccessPolicy   string `json:"ap"`
	ContentType    string `json:"ct,omitempty"`
	Ext            string `json:"ext,omitempty"`
	FileName       string `json:"fn,omitempty"`
	Provider       string `json:"pv"`
	Bucket         string `json:"bk,omitempty"`
	StorePath      string `json:"sp,omitempty"`
	Staged         bool   `json:"staged,omitempty"`
	MaxSize        int64  `json:"mx"`
	PartSize       int64  `json:"ps"`
	IdempotencyKey string `json:"idk,omitempty"`
	Shared         bool   `json:"sh,omitempty"`
	ExpireAt       int64  `json:"exp"`
}

func (m *DefaultManager) signMultipartTicket(ticket *multipartTicket) (string, error) {
	secretKey, err := m.signedURLSecret()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(ticket)
	if err != nil {
		return "", fmt.Errorf("marshal multipart ticket: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + directTicketSignature(payload, secretKey), nil
}

func (m *DefaultManager) parseMultipartTicket(raw string) (*multipartTicket, error) {
	secretKey, err := m.signedURLSecret()
	if err != nil {
		return nil, err
	}
	payload, signature, ok := strings.Cut(strings.TrimSpace(raw), ".")
	if !ok || payload == "" || signature == "" {
		return nil, ErrMultipartTicketInvalid
	}
	// 恒定时间比较，与签名 URL 和直传票据的验签口径保持一致。
	if !hmac.Equal([]byte(directTicketSignature(payload, secretKey)), []byte(signature)) {
		return nil, ErrMultipartTicketInvalid
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, ErrMultipartTicketInvalid
	}
	ticket := &multipartTicket{}
	if err = json.Unmarshal(decoded, ticket); err != nil {
		return nil, ErrMultipartTicketInvalid
	}
	// 先验签再判过期：过期信息只对持有合法票据的调用方暴露。
	if time.Now().Unix() > ticket.ExpireAt {
		return nil, ErrMultipartTicketExpired
	}
	return ticket, nil
}

// InitMultipartUpload 开启一次分片上传。
//
// 能在这一步做的校验只有「声明值」一层——扩展名、MIME、public 的危险类型，
// 以及按声明总大小做的体积与配额预检。真实内容要到 Complete 才齐，
// 那时会按实际累计大小再卡一次。这个信任缺口是分片上传固有的，
// 不是实现偷懒：客户端本来就可以先声明 1MB 再传 1GB。
func (m *DefaultManager) InitMultipartUpload(ctx context.Context, req *model.MultipartInitRequest) (*model.MultipartSession, error) {
	if !m.config.Manager.EnableMultipartUpload {
		return nil, ErrMultipartDisabled
	}
	uploader, ok := m.provider.(provider.MultipartUploader)
	if !ok {
		return nil, provider.ErrMultipartUnsupported
	}
	upload := &model.UploadRequest{
		Namespace:    req.Namespace,
		AccountID:    req.AccountID,
		BizType:      req.BizType,
		FileName:     req.FileName,
		ContentType:  req.ContentType,
		AccessPolicy: req.AccessPolicy,
		Creator:      req.Creator,
	}
	if err := provider.ValidateIdentitySegments(upload); err != nil {
		return nil, err
	}
	if err := validateMetadataMap(req.Metadata); err != nil {
		return nil, err
	}
	ext := resolveExt(req.FileName)
	// 分片上传拿不到内容，MIME 只能认声明值——与直传同一条路径，
	// 所以这里复用 validateDeclared 而不是 validateUpload。
	// 内容拿不到，MIME 只能认声明值——与直传同一条路径。
	mimeType := req.ContentType
	if mimeType == "" {
		mimeType = mime.TypeByExtension(ext)
	}
	if err := m.validateUpload(ext, mimeType); err != nil {
		return nil, err
	}
	accessPolicy := m.accessPolicy(upload)
	// body 传 nil：与直传一样，这一步只能按声明类型判断。
	if err := m.validatePublicContent(accessPolicy, mimeType, ext, nil); err != nil {
		return nil, err
	}
	staged := m.contentInspector() != nil && accessPolicy == model.AccessPolicyPublic
	maxSize := m.maxFileSize()
	if req.TotalSize < 0 || req.TotalSize > maxSize {
		return nil, fmt.Errorf("%w: declared %d bytes exceeds the limit %d", ErrFileTooLarge, req.TotalSize, maxSize)
	}
	if err := m.checkQuota(ctx, req.Namespace, req.AccountID, req.TotalSize); err != nil {
		return nil, err
	}

	upload.Ext = ext
	// object key 的随机后缀依赖 Hash 字段做前缀，分片上传此时还没有内容，
	// 用一个一次性 ID 顶上——它只影响 key 的可读性，不参与任何校验。
	upload.Hash = xid.New().String()
	upload.AccessPolicy = accessPolicy
	if staged {
		upload.AccessPolicy = model.AccessPolicyPrivate
	}

	init, err := uploader.InitMultipart(ctx, upload)
	if err != nil {
		return nil, err
	}
	expireSeconds := req.ExpireSeconds
	if expireSeconds <= 0 {
		expireSeconds = m.config.Manager.MultipartExpire
	}
	expireAt := time.Now().Add(time.Duration(expireSeconds) * time.Second)
	partSize := normalizePartSize(req.PartSize)

	ticket := &multipartTicket{
		ObjectKey:      init.ObjectKey,
		UploadID:       init.UploadID,
		Namespace:      req.Namespace,
		AccountID:      req.AccountID,
		Creator:        req.Creator,
		BizType:        req.BizType,
		AccessPolicy:   accessPolicy,
		ContentType:    mimeType,
		Ext:            ext,
		FileName:       req.FileName,
		Provider:       init.Provider,
		Bucket:         init.Bucket,
		StorePath:      init.StorePath,
		MaxSize:        maxSize,
		Staged:         staged,
		PartSize:       partSize,
		IdempotencyKey: req.IdempotencyKey,
		Shared:         req.Shared && req.IsAdmin,
		ExpireAt:       expireAt.Unix(),
	}
	signed, err := m.signMultipartTicket(ticket)
	if err != nil {
		// 票据签不出来，这次会话就谁也用不了了，顺手把 OSS 侧的
		// uploadID 也放弃掉——留着它会一直按已上传分片计费。
		_ = uploader.AbortMultipart(ctx, init.ObjectKey, init.UploadID)
		return nil, err
	}
	return &model.MultipartSession{
		Ticket:    signed,
		UploadID:  init.UploadID,
		PartSize:  partSize,
		MaxParts:  model.MaxPartNumber,
		ExpireAt:  expireAt,
		ObjectKey: init.ObjectKey,
	}, nil
}

// UploadPart 上传一个分片。
//
// 身份比对不能省：票据是不记名字符串，泄露出去就是一次可用的写入许可。
// 与直传登记同一套校验。
func (m *DefaultManager) UploadPart(ctx context.Context, rawTicket string, partNumber int,
	body io.Reader, size int64, req *model.AccessRequest) (*model.MultipartPart, error) {
	if !m.config.Manager.EnableMultipartUpload {
		return nil, ErrMultipartDisabled
	}
	uploader, ok := m.provider.(provider.MultipartUploader)
	if !ok {
		return nil, provider.ErrMultipartUnsupported
	}
	ticket, err := m.parseMultipartTicket(rawTicket)
	if err != nil {
		return nil, err
	}
	if err = m.authorizeTicket(ticket, req); err != nil {
		return nil, err
	}
	return uploader.UploadPart(ctx, ticket.ObjectKey, ticket.UploadID, partNumber, body, size)
}

// CompleteMultipartUpload 合并分片并登记元数据。
func (m *DefaultManager) CompleteMultipartUpload(ctx context.Context, req *model.MultipartCompleteRequest) (*model.UploadResult, error) {
	if !m.config.Manager.EnableMultipartUpload {
		return nil, ErrMultipartDisabled
	}
	uploader, ok := m.provider.(provider.MultipartUploader)
	if !ok {
		return nil, provider.ErrMultipartUnsupported
	}
	if req == nil || len(req.Parts) == 0 {
		return nil, ErrNoParts
	}
	if err := validateMetadataMap(req.Metadata); err != nil {
		return nil, err
	}
	ticket, err := m.parseMultipartTicket(req.Ticket)
	if err != nil {
		return nil, err
	}
	identity := &model.AccessRequest{Namespace: req.Namespace, AccountID: req.AccountID, UserID: req.UserID}
	if err = m.authorizeTicket(ticket, identity); err != nil {
		return nil, err
	}
	if err = validateParts(req.Parts); err != nil {
		return nil, err
	}

	id := completedUploadID("multipart", ticket.ObjectKey)
	if result, err := m.completedUpload(ctx, id, ticket.Creator, req.ExpireSeconds); result != nil || err != nil {
		return result, err
	}
	if result, err := m.previousUpload(ctx, ticket.ObjectKey, ticket.Namespace, ticket.AccountID, ticket.Creator, req.ExpireSeconds); result != nil || err != nil {
		return result, err
	}
	if m.contentInspector() != nil && !ticket.Staged && ticket.AccessPolicy == model.AccessPolicyPublic {
		return nil, ErrPublicUploadRequiresInspection
	}
	upload := &model.UploadRequest{Namespace: ticket.Namespace, AccountID: ticket.AccountID, Creator: ticket.Creator, IdempotencyKey: ticket.IdempotencyKey, ExpireSeconds: req.ExpireSeconds}
	if existing, hit, err := m.findIdempotent(ctx, upload); err != nil {
		return nil, err
	} else if hit {
		return m.idempotentResult(ctx, existing, upload)
	}

	_, completionErr := uploader.CompleteMultipart(ctx, ticket.ObjectKey, ticket.UploadID, req.Parts)
	stat, err := m.provider.Stat(ctx, ticket.ObjectKey)
	if err != nil {
		return nil, errors.Join(completionErr, err)
	}
	if stat == nil || !stat.Exists {
		if completionErr != nil {
			return nil, completionErr
		}
		return nil, ErrDirectUploadObjectMissing
	}
	if stat.Size > ticket.MaxSize {
		_ = m.provider.Delete(ctx, ticket.ObjectKey)
		return nil, fmt.Errorf("%w: %d bytes exceeds the limit %d", ErrFileTooLarge, stat.Size, ticket.MaxSize)
	}
	if err = m.checkQuota(ctx, ticket.Namespace, ticket.AccountID, stat.Size); err != nil {
		return nil, err
	}

	now := time.Now()
	resource := &model.Resource{
		ID:                  id,
		Namespace:           ticket.Namespace,
		AccountID:           ticket.AccountID,
		Name:                ticket.FileName,
		Ext:                 ticket.Ext,
		MimeType:            ticket.ContentType,
		Size:                stat.Size,
		Provider:            ticket.Provider,
		Bucket:              ticket.Bucket,
		ObjectKey:           ticket.ObjectKey,
		StorePath:           ticket.StorePath,
		Scheme:              m.config.Provider.Scheme,
		Status:              model.ResourceStatusActive,
		BizType:             ticket.BizType,
		Tags:                normalizeTags(req.Tags),
		AccessPolicy:        ticket.AccessPolicy,
		HotlinkProtection:   m.config.Access.EnableHotlinkProtection,
		Creator:             ticket.Creator,
		Metadata:            req.Metadata,
		IdempotencyKey:      ticket.IdempotencyKey,
		IdempotencyExpireAt: m.idempotencyExpireAt(ticket.IdempotencyKey, now),
		Shared:              ticket.Shared,
		Version:             1,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	// Hash 刻意留空：分片上传的内容服务端从未完整经手，
	// 算哈希要把整个对象再读一遍。与直传同一个取舍——
	// 代价是这类资源不参与内容去重。
	if ticket.Staged || m.contentInspector() != nil {
		if _, err = m.readAndInspectObject(ctx, resource, ticket.MaxSize); err != nil {
			return nil, err
		}
	}
	resultURL, err := m.uploadAccessURL(ctx, resource, upload)
	if err != nil {
		return nil, err
	}
	if ticket.Staged {
		if err = m.applyObjectACL(ctx, resource.ObjectKey, ticket.AccessPolicy); err != nil {
			return nil, err
		}
	}
	if err = m.store.Create(ctx, resource); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			if result, lookupErr := m.completedUpload(ctx, id, ticket.Creator, req.ExpireSeconds); result != nil || lookupErr != nil {
				return result, lookupErr
			}
			if existing, hit, lookupErr := m.findIdempotent(ctx, upload); lookupErr == nil && hit {
				return m.idempotentResult(ctx, existing, upload)
			}
		}
		return nil, err
	}
	m.addUsage(ctx, resource.Namespace, resource.AccountID, resource.Size, 1)
	m.countUpload(resource, true)
	return &model.UploadResult{Resource: m.fillURL(resource), Uploaded: true, URL: resultURL}, nil
}

// AbortMultipartUpload 放弃一次分片上传并清理已传分片。
//
// 客户端主动放弃时应当调它。不调也不会永远泄漏——OSS 侧可以配
// 生命周期规则自动清理未完成的分片，本地实现则靠票据过期后的目录清理。
// 但「可以兜底」不等于「不用调」：未完成的分片一直在计费。
func (m *DefaultManager) AbortMultipartUpload(ctx context.Context, rawTicket string, req *model.AccessRequest) error {
	uploader, ok := m.provider.(provider.MultipartUploader)
	if !ok {
		return provider.ErrMultipartUnsupported
	}
	ticket, err := m.parseMultipartTicket(rawTicket)
	if err != nil {
		return err
	}
	if err = m.authorizeTicket(ticket, req); err != nil {
		return err
	}
	return uploader.AbortMultipart(ctx, ticket.ObjectKey, ticket.UploadID)
}

// authorizeTicket 校验票据里签着的身份与当前调用方一致。
//
// 全等比较，与 authorizeScope 同一口径：票据泄露出去之后，
// 拿到它的人必须同时也在同一个租户、同一个用户身份下，才能继续用。
func (m *DefaultManager) authorizeTicket(ticket *multipartTicket, req *model.AccessRequest) error {
	if req == nil {
		return nil
	}
	if ticket.Namespace != req.Namespace || ticket.AccountID != req.AccountID {
		return ErrDirectUploadIdentityMismatch
	}
	if ticket.Creator != "" && req.UserID != "" && ticket.Creator != req.UserID {
		return ErrDirectUploadIdentityMismatch
	}
	return nil
}

// validateParts 校验分片清单。
//
// 非末片必须达到对象存储要求的下限（5MB）。这一条在本地 provider 上
// 也强制执行：开发时能传的分片到了线上被 OSS 拒掉，是最难查的那类问题。
func validateParts(parts []model.MultipartPart) error {
	ordered := append([]model.MultipartPart(nil), parts...)
	maxNumber := 0
	for _, part := range ordered {
		if part.PartNumber < 1 || part.PartNumber > model.MaxPartNumber {
			return fmt.Errorf("%w: %d", provider.ErrPartNumberInvalid, part.PartNumber)
		}
		if part.PartNumber > maxNumber {
			maxNumber = part.PartNumber
		}
	}
	for _, part := range ordered {
		if part.PartNumber == maxNumber {
			continue
		}
		if part.Size > 0 && part.Size < model.MinPartSize {
			return fmt.Errorf("%w: part %d is %d bytes, minimum is %d",
				ErrPartTooSmall, part.PartNumber, part.Size, model.MinPartSize)
		}
	}
	return nil
}

func normalizePartSize(partSize int64) int64 {
	if partSize <= 0 {
		return model.DefaultPartSize
	}
	if partSize < model.MinPartSize {
		return model.MinPartSize
	}
	if partSize > model.MaxPartSize {
		return model.MaxPartSize
	}
	return partSize
}

// MultipartUploader 是 transport 层消费的分片上传能力。
//
// 与 DirectUploader 同样做成独立接口而不是并进 Manager：
// 这是一条可选链路，自定义 Manager 实现没有义务提供它，
// 缺了只应该让那四条路由不注册，而不是让整个接入层编译不过。
type MultipartUploader interface {
	InitMultipartUpload(ctx context.Context, req *model.MultipartInitRequest) (*model.MultipartSession, error)
	UploadPart(ctx context.Context, ticket string, partNumber int, body io.Reader, size int64, req *model.AccessRequest) (*model.MultipartPart, error)
	CompleteMultipartUpload(ctx context.Context, req *model.MultipartCompleteRequest) (*model.UploadResult, error)
	AbortMultipartUpload(ctx context.Context, ticket string, req *model.AccessRequest) error
}

var _ MultipartUploader = (*DefaultManager)(nil)
