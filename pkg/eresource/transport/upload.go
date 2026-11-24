package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

// ErrUploadBusy 上传并发槽位已满。
var ErrUploadBusy = errors.New("eresource: too many concurrent uploads")

// Upload 处理 multipart/form-data 上传。
func (h *HTTPHandler) Upload(w http.ResponseWriter, r *http.Request) {
	if !h.acquireUploadSlot() {
		h.failBusy(w, r)
		return
	}
	defer h.releaseUploadSlot()

	// 刻意不把 max_file_size 当作 maxMemory 传进去。
	// ParseMultipartForm 会把 maxMemory 以内的文件整份留在内存里，而
	// readUploadBody 随后还要 ReadAll 一份——单次上传的峰值因此是文件大小的
	// 两倍，再乘上并发数就是进程的内存上界。
	// 传一个小值让文件自然落到临时盘，峰值回到一份。代价是每次上传多一次
	// 临时文件的写与读，用确定的磁盘 I/O 换掉不确定的 OOM。
	if err := r.ParseMultipartForm(uploadFormMemory); err != nil {
		h.fail(w, r, http.StatusBadRequest, fmt.Errorf("parse multipart form: %w", err))
		return
	}
	file, header, err := r.FormFile(FormFieldFile)
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, fmt.Errorf("read form file %q: %w", FormFieldFile, err))
		return
	}
	defer file.Close()

	req := &model.UploadRequest{
		FileName:     header.Filename,
		ContentType:  header.Header.Get("Content-Type"),
		Reader:       file,
		BizType:      h.formValue(r, "biz_type"),
		Tags:         h.formValues(r, "tags"),
		AccessPolicy: h.formValue(r, "access_policy"),
		Metadata:     h.prefixedForm(r, metadataFormPrefix),
		// shared 只是请求，是否采纳由 Manager 按 IsAdmin 决定。
		Shared:         boolForm(h.formValue(r, "shared")),
		IdempotencyKey: strings.TrimSpace(r.Header.Get(HeaderIdempotencyKey)),
	}
	// 租户与创建者只认请求头，与读取侧同一套信任模型
	if err := h.applyUploadIdentity(r, req); err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	if raw := h.formValue(r, "expire_seconds"); raw != "" {
		if expire, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			req.ExpireSeconds = expire
		}
	}

	// 上传到对象存储和登记元数据需要独立于请求的时间预算。
	// 同时切断取消链——客户端在落盘与登记之间断开时，若连带取消整条链路，
	// 对象已经写进去、store.Create 却没跑完，留下的是一个谁也查不到、
	// 也不会被任何清理逻辑回收的孤儿对象。
	uploadCtx, cancel := h.uploadContext(r)
	defer cancel()

	result, err := h.manager.Upload(uploadCtx, req)
	if err != nil {
		h.fail(w, r, uploadStatus(err), err)
		return
	}
	h.ok(w, result.View())
}

// uploadContext 返回一条脱离请求超时的上传上下文。
//
// 注意它解决不了「读请求体本身超时」——请求体的读取受 HTTP Server 的
// ReadTimeout 约束。
// 这里保证的是：内容一旦读进来，后续的落盘与登记有完整的预算跑完。
func (h *HTTPHandler) uploadContext(r *http.Request) (context.Context, context.CancelFunc) {
	timeout := h.cfg.Manager.UploadTimeoutSeconds
	if timeout <= 0 {
		timeout = config.DefaultUploadTimeoutSec
	}
	return context.WithTimeout(context.WithoutCancel(r.Context()), time.Duration(timeout)*time.Second)
}

// acquireUploadSlot 获取一个上传并发槽位，拿不到返回 false。
//
// 上传时 ParseMultipartForm 会把 MaxFileSize 以内的内容留在内存里、
// 超出部分落临时盘，两者都随并发数线性增长：没有闸门时内存上界
// 就是 MaxFileSize × 并发数（默认 32MB × 并发），C 端放量足以直接打到 OOM。
//
// 满了立刻返回而不是排队：排队只是把压力从内存挪到连接数上，
// 而内存才是这里真正的瓶颈，让调用方拿 503 退避重试更诚实。
func (h *HTTPHandler) acquireUploadSlot() bool {
	if h.uploadSlots == nil {
		return true
	}
	select {
	case h.uploadSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *HTTPHandler) releaseUploadSlot() {
	if h.uploadSlots == nil {
		return
	}
	<-h.uploadSlots
}

// failBusy 返回 503 并带上 Retry-After，提示调用方这是瞬时拥塞而非永久失败。
func (h *HTTPHandler) failBusy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "1")
	h.fail(w, r, http.StatusServiceUnavailable, ErrUploadBusy)
}

func uploadStatus(err error) int {
	switch {
	case errors.Is(err, manager.ErrEmptyUpload),
		errors.Is(err, manager.ErrExtNotAllowed),
		errors.Is(err, manager.ErrMimeNotAllowed),
		errors.Is(err, manager.ErrMimeMismatch),
		// metadata 的 key 非法：这是调用方把参数写错了，不是服务端故障。
		errors.Is(err, model.ErrInvalidQuery),
		errors.Is(err, provider.ErrInvalidSegment):
		return http.StatusBadRequest
	case errors.Is(err, manager.ErrFileTooLarge):
		return http.StatusRequestEntityTooLarge
	// 507 而不是 413：413 说的是「这个文件太大」，客户端换个小文件就能过；
	// 配额用尽换小文件也没用，是服务端这一侧的空间不够了。
	case errors.Is(err, manager.ErrQuotaExceeded):
		return http.StatusInsufficientStorage
	case errors.Is(err, store.ErrDuplicate), errors.Is(err, manager.ErrPublicUploadRequiresInspection):
		return http.StatusConflict
	case errors.Is(err, manager.ErrContentRejected):
		return http.StatusUnprocessableEntity
	case errors.Is(err, manager.ErrUnsafePublicContent):
		return http.StatusUnsupportedMediaType
	default:
		return http.StatusInternalServerError
	}
}
