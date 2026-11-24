package transport

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// DirectUploadPolicy 签发前端直传凭证。
//
// 只在 manager.enable_direct_upload 打开且 provider 支持直传时注册。
// 响应里的 form_fields 必须原样提交，文件字段放在最后。
func (h *HTTPHandler) DirectUploadPolicy(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	req := &model.DirectUploadRequest{
		// 身份三件套只认 IdentityResolver 的结果，与中转上传同一套口径：
		// 取自表单的话，调用方改个 account_id 就能拿到一张写进别人租户的凭证。
		Namespace:    identity.Namespace,
		AccountID:    identity.AccountID,
		Creator:      identity.UserID,
		BizType:      h.directValue(r, "biz_type"),
		FileName:     h.directValue(r, "file_name"),
		ContentType:  h.directValue(r, "content_type"),
		AccessPolicy: h.directValue(r, "access_policy"),
	}
	policy, err := h.directUploader.CreateDirectUpload(r.Context(), req)
	if err != nil {
		h.fail(w, r, directStatus(err), err)
		return
	}
	h.ok(w, policy)
}

// DirectUploadComplete 在客户端直传完成后登记元数据。
//
// 不登记的对象等于一份查不到、也不会被回收的垃圾，因此这一步是直传流程
// 的必经环节，而不是可选的「通知」。
func (h *HTTPHandler) DirectUploadComplete(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	ticket := h.directValue(r, "ticket")
	if ticket == "" {
		h.fail(w, r, http.StatusBadRequest, errors.New("missing 'ticket' parameter"))
		return
	}
	req := &model.DirectUploadCompleteRequest{
		Ticket:    ticket,
		Namespace: identity.Namespace,
		AccountID: identity.AccountID,
		Creator:   identity.UserID,
		FileName:  h.directValue(r, "file_name"),
		// 标签与 metadata 在登记这一步才写入：签凭证时客户端还没传完文件，
		// 这些字段也不参与 policy 的任何约束，签进票据只会让票据变长。
		Tags:     h.formValues(r, "tags"),
		Metadata: h.prefixedForm(r, metadataFormPrefix),
	}
	if raw := h.directValue(r, "expire_seconds"); raw != "" {
		if expire, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			req.ExpireSeconds = expire
		}
	}
	// 登记只是写一条元数据，不占上传并发槽位，但要和上传一样脱离 3 秒请求超时：
	// 这里会先向对象存储 Stat 一次确认文件真的传上去了。
	completeCtx, cancel := h.uploadContext(r)
	defer cancel()

	result, err := h.directUploader.CompleteDirectUpload(completeCtx, req)
	if err != nil {
		h.fail(w, r, directStatus(err), err)
		return
	}
	h.ok(w, result.View())
}

// directValue 先取表单再取查询串。
// 直传两个接口没有文件体，调用方用 form 或 query 提交都很常见，两种都接受。
func (h *HTTPHandler) directValue(r *http.Request, key string) string {
	if value := h.formValue(r, key); value != "" {
		return value
	}
	return h.queryValue(r, key)
}

// directStatus 映射前端直传两个接口的错误。
func directStatus(err error) int {
	switch {
	case errors.Is(err, manager.ErrDirectUploadDisabled),
		errors.Is(err, manager.ErrDirectUploadUnsupported):
		return http.StatusNotImplemented
	// 票据无效 / 过期是调用方持有的凭证问题，不是参数写错，
	// 与签名地址的口径保持一致：伪造回 403，过期回 410。
	case errors.Is(err, manager.ErrDirectUploadTicketInvalid):
		return http.StatusForbidden
	case errors.Is(err, manager.ErrDirectUploadTicketExpired):
		return http.StatusGone
	case errors.Is(err, manager.ErrDirectUploadIdentityMismatch):
		return http.StatusForbidden
	// 票据有效但对象不在：直传没真正完成，客户端应当重传而不是重试登记。
	case errors.Is(err, manager.ErrDirectUploadObjectMissing):
		return http.StatusConflict
	default:
		return uploadStatus(err)
	}
}
