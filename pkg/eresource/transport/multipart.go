package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// 分片上传的表单字段。
const (
	FormFieldTicket     = "ticket"
	FormFieldPartNumber = "part_number"
	FormFieldParts      = "parts"
)

// partFormMemory 分片接收时留在内存里的上限。
//
// 与普通上传同一个理由（见 uploadFormMemory）：分片本身就有 5MB 下限，
// 全留在内存里意味着单次请求峰值至少 10MB，再乘并发数就是进程的内存上界。
const partFormMemory = 1 << 20

// MultipartInit 开启一次分片上传，返回会话票据。
func (h *HTTPHandler) MultipartInit(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	req := &model.MultipartInitRequest{
		Namespace:    identity.Namespace,
		AccountID:    identity.AccountID,
		Creator:      identity.UserID,
		IsAdmin:      identity.IsAdmin,
		FileName:     h.formValue(r, "file_name"),
		ContentType:  h.formValue(r, "content_type"),
		BizType:      h.formValue(r, "biz_type"),
		AccessPolicy: h.formValue(r, "access_policy"),
		Tags:         h.formValues(r, "tags"),
		Metadata:     h.prefixedForm(r, metadataFormPrefix),
		Shared:       boolForm(h.formValue(r, "shared")),
		// 幂等键与普通上传同一个头：一次分片上传是一个操作，
		// 中途重试 init 不该产生第二个 object key。
		IdempotencyKey: strings.TrimSpace(r.Header.Get(HeaderIdempotencyKey)),
	}
	if raw := h.formValue(r, "total_size"); raw != "" {
		if size, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			req.TotalSize = size
		}
	}
	if raw := h.formValue(r, "part_size"); raw != "" {
		if size, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			req.PartSize = size
		}
	}
	if raw := h.formValue(r, "expire_seconds"); raw != "" {
		if expire, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			req.ExpireSeconds = expire
		}
	}

	session, err := h.multipartUploader.InitMultipartUpload(r.Context(), req)
	if err != nil {
		h.fail(w, r, multipartStatus(err), err)
		return
	}
	h.ok(w, session)
}

// MultipartPart 接收一个分片。
func (h *HTTPHandler) MultipartPart(w http.ResponseWriter, r *http.Request) {
	if !h.acquireUploadSlot() {
		h.failBusy(w, r)
		return
	}
	defer h.releaseUploadSlot()

	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	if err = r.ParseMultipartForm(partFormMemory); err != nil {
		h.fail(w, r, http.StatusBadRequest, fmt.Errorf("parse multipart form: %w", err))
		return
	}
	ticket := h.formValue(r, FormFieldTicket)
	if ticket == "" {
		h.fail(w, r, http.StatusBadRequest, errors.New("ticket is required"))
		return
	}
	partNumber, err := strconv.Atoi(h.formValue(r, FormFieldPartNumber))
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, fmt.Errorf("invalid %s: %w", FormFieldPartNumber, err))
		return
	}
	file, header, err := r.FormFile(FormFieldFile)
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, fmt.Errorf("read form file %q: %w", FormFieldFile, err))
		return
	}
	defer file.Close()

	// 与 Upload 同一套：分片推到对象存储的耗时远超 ehttp 给单个请求的预算，
	// 且不能让客户端断连连带取消——那会留下一个只传了一半的分片。
	uploadCtx, cancel := h.uploadContext(r)
	defer cancel()

	part, err := h.multipartUploader.UploadPart(uploadCtx, ticket, partNumber, file, header.Size, identity)
	if err != nil {
		h.fail(w, r, multipartStatus(err), err)
		return
	}
	h.ok(w, part)
}

// MultipartComplete 合并分片并登记元数据。
func (h *HTTPHandler) MultipartComplete(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	rawParts := h.formValue(r, FormFieldParts)
	if rawParts == "" {
		h.fail(w, r, http.StatusBadRequest, fmt.Errorf("%s is required", FormFieldParts))
		return
	}
	var parts []model.MultipartPart
	if err = json.Unmarshal([]byte(rawParts), &parts); err != nil {
		h.fail(w, r, http.StatusBadRequest, fmt.Errorf("invalid %s: %w", FormFieldParts, err))
		return
	}
	req := &model.MultipartCompleteRequest{
		Ticket:    h.formValue(r, FormFieldTicket),
		Namespace: identity.Namespace,
		AccountID: identity.AccountID,
		UserID:    identity.UserID,
		Parts:     parts,
		Tags:      h.formValues(r, "tags"),
		Metadata:  h.prefixedForm(r, metadataFormPrefix),
	}
	if raw := h.formValue(r, "expire_seconds"); raw != "" {
		if expire, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			req.ExpireSeconds = expire
		}
	}
	uploadCtx, cancel := h.uploadContext(r)
	defer cancel()

	result, err := h.multipartUploader.CompleteMultipartUpload(uploadCtx, req)
	if err != nil {
		h.fail(w, r, multipartStatus(err), err)
		return
	}
	h.ok(w, result.View())
}

// MultipartAbort 放弃一次分片上传。
func (h *HTTPHandler) MultipartAbort(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	if err = h.multipartUploader.AbortMultipartUpload(r.Context(), h.formValue(r, FormFieldTicket), identity); err != nil {
		h.fail(w, r, multipartStatus(err), err)
		return
	}
	h.ok(w, map[string]bool{"aborted": true})
}

// multipartStatus 把分片上传的错误映射成状态码。
//
// 票据相关的错误刻意都落在 4xx：它们表达的都是「你这张票据不行」，
// 对调用方来说是可自助纠正的（重新 init），不是服务端故障。
func multipartStatus(err error) int {
	switch {
	case errors.Is(err, manager.ErrMultipartDisabled),
		errors.Is(err, provider.ErrMultipartUnsupported):
		return http.StatusNotImplemented
	case errors.Is(err, manager.ErrMultipartTicketInvalid),
		errors.Is(err, provider.ErrUploadSessionInvalid):
		return http.StatusBadRequest
	case errors.Is(err, manager.ErrMultipartTicketExpired):
		return http.StatusGone
	case errors.Is(err, manager.ErrDirectUploadIdentityMismatch):
		return http.StatusForbidden
	case errors.Is(err, manager.ErrNoParts),
		errors.Is(err, manager.ErrPartTooSmall),
		errors.Is(err, provider.ErrPartNumberInvalid):
		return http.StatusBadRequest
	default:
		return uploadStatus(err)
	}
}
