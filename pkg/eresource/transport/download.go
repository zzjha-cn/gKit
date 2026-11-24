package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// Content 代理下载：由业务服务完成鉴权后回吐内容，不向客户端暴露底层地址。
func (h *HTTPHandler) Content(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue(paramID)
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	// 一次查库完成取元数据与访问校验：分开调 Get + VerifyAccess 要查两次库，
	// 而这是每次下载都要付的成本。
	resource, err := h.manager.GetVerified(ctx, id, identity)
	if err != nil {
		h.fail(w, r, accessStatus(err), err)
		return
	}
	// 审计在回吐内容**之前**记录。
	//
	// 放到传完之后就太晚了——fail-closed 的语义是「没记下就不给看」，
	// 而响应体一旦开始写就收不回来。代价是这里记的 Bytes 是对象大小
	// 而不是实际传输量（Range 请求、客户端中途断开都会让两者不等），
	// 实际传输量由指标那条路负责（CountDownload 统计的是写出的字节）。
	if err = h.manager.AuditDownload(ctx, resource, identity, routeProxy, resource.Size); err != nil {
		h.fail(w, r, http.StatusServiceUnavailable, err)
		return
	}
	// 大文件改走 302：鉴权已经在上面做完了，这里只是不再由业务服务回吐字节。
	if h.manager.ShouldRedirectContent(resource) {
		userID := ""
		if identity != nil {
			userID = identity.UserID
		}
		redirectURL, redirectErr := h.manager.RedirectURL(ctx, resource, userID)
		if redirectErr != nil {
			// 签不出地址就老实走代理，别让一次成本优化变成一次功能故障。
			slog.ErrorContext(r.Context(), "eresource: sign redirect URL", "id", resource.ID, "err", redirectErr)
		} else {
			h.countDownload(resource.ObjectKey,
				downloadLabels{namespace: resource.Namespace, policy: resource.AccessPolicy, route: routeRedirect}, 0)
			http.Redirect(w, r, redirectURL, http.StatusFound)
			return
		}
	}
	h.stream(w, r, resource.ObjectKey, resource.Name, resource.MimeType,
		downloadLabels{namespace: resource.Namespace, policy: resource.AccessPolicy, route: routeProxy})
}

// SignedContent 校验签名 URL 后回吐内容。
// 验签后检查对象是否仍有有效资源引用。
func (h *HTTPHandler) SignedContent(w http.ResponseWriter, r *http.Request) {
	objectKey := strings.Trim(r.PathValue(paramObjectKey), "/")
	if objectKey == "" {
		h.fail(w, r, http.StatusBadRequest, errors.New("object key is empty"))
		return
	}
	query := r.URL.Query()
	// 请求方身份只用于 private 资源的绑定校验；protected 的签名地址仍是
	// bearer token，身份缺失不影响它。
	//
	// 这里也走解析器而不是直接读请求头：留一个硬编码入口的话，
	// 换了解析器的接入方会发现 private 的绑定校验仍在看那个它根本没在用的头。
	// 但签名路由不参与 require_identity——签名本身就是凭证，
	// 它要能被 <img src> 直接加载，那种场景下没有任何身份头。
	requester := ""
	if identity, err := h.resolveIdentity(r); err == nil && identity != nil {
		requester = identity.UserID
	}
	if err := h.manager.VerifySignedObject(objectKey, query, requester); err != nil {
		h.fail(w, r, signatureStatus(err), err)
		return
	}
	if manager, ok := h.manager.(interface {
		VerifySignedObjectActive(context.Context, string) error
	}); ok {
		if err := manager.VerifySignedObjectActive(r.Context(), objectKey); err != nil {
			h.fail(w, r, accessStatus(err), err)
			return
		}
	}
	if err := h.manager.AllowSignedAccess(objectKey, h.clientIP(r)); err != nil {
		h.fail(w, r, accessStatus(err), err)
		return
	}
	h.stream(w, r, objectKey, path.Base(objectKey), "",
		downloadLabels{route: routeSigned})
}

// downloadLabels 携带下载指标的维度。
//
// 独立成结构体而不是加三个参数：这几个维度还会继续长（Roadmap.md §4.1
// 的 B12 成本分布就需要它们），一路加参数会让 stream 的签名越来越难读。
type downloadLabels struct {
	namespace string
	policy    string
	route     string
}

// 下载路径标识。B12 关心的「流量到底走了哪条路」就靠这个维度区分。
const (
	routeProxy  = "proxy"  // 业务服务代理读流，出口流量记在业务服务头上
	routeSigned = "signed" // 签名地址回源到本服务的验签路由
	// routeRedirect 大文件 302 分流：业务服务只付一次跳转，字节走对象存储 / CDN。
	// 它的 download_bytes 恒为 0 正是重点——这正是分流省下来的那部分出口流量。
	routeRedirect = "redirect"
)

// downloadCounter 由支持下载埋点的 Manager 实现。
//
// 做成可选接口而不是写进 manager.Manager：指标是横切关注点，
// 自定义 Manager 实现没有义务提供它，缺了也不该让整个接入层编译不过。
// 与 provider.ObjectReader / RangeReader 同一套做法。
type downloadCounter interface {
	CountDownload(namespace, policy, route, objectKey string, bytes int64)
}

// countDownload 在内容写完之后上报。
//
// 统计的是**实际写出的字节**而不是对象大小：Range 请求只传了一段，
// 客户端中途断开更是只传了一部分，按对象大小记账会把带宽账单记虚。
func (h *HTTPHandler) countDownload(objectKey string, labels downloadLabels, written int64) {
	counter, ok := h.manager.(downloadCounter)
	if !ok {
		return
	}
	counter.CountDownload(labels.namespace, labels.policy, labels.route, objectKey, written)
}

func (h *HTTPHandler) stream(w http.ResponseWriter, r *http.Request, objectKey, fileName, mimeType string, labels downloadLabels) {
	// 回吐对象内容的耗时取决于文件大小与带宽，不能受 调用方的请求超时约束。
	// 直接用 r.Context() 的话，AliOSSProvider.Open 带着这个 ctx 去读 OSS，
	// 3 秒一到读流就被取消——此时响应头早已发出，客户端拿到的是个截断的文件，
	// 服务端只有一行日志。不报错、不重试的静默数据损坏，比直接失败更难发现。
	//
	// 切断取消链之后客户端断连不再取消 ctx，但这不会造成泄漏：
	// 连接断开后向 w 的写入会立刻报错，io.Copy 随之返回，reader 照常关闭。
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(r.Context()),
		time.Duration(h.cfg.Access.DownloadTimeoutSeconds)*time.Second,
	)
	defer cancel()

	// HEAD 只回响应头，不读内容。播放器与下载工具拿它探测体积与是否支持续传，
	// 为此把整个对象拉一遍（甚至只是打开一次 OSS 读流）是纯粹的浪费。
	if r.Method == http.MethodHead {
		h.headObject(w, r, ctx, objectKey, fileName, mimeType)
		return
	}

	reader, stat, err := h.openForRequest(w, r, ctx, objectKey)
	if err != nil {
		h.failStream(w, r, err)
		return
	}
	defer reader.Close()

	h.writeContentHeaders(w, objectKey, fileName, mimeType, statSize(stat))
	written, err := io.Copy(w, reader)
	if err != nil {
		slog.ErrorContext(r.Context(), "eresource: stream object", "object_key", objectKey, "err", err)
	}
	// 失败也要上报：传到一半断掉的那部分字节一样走了出口带宽。
	h.countDownload(objectKey, labels, written)
}

// headObject 响应 HEAD：只写响应头，不碰内容。
func (h *HTTPHandler) headObject(w http.ResponseWriter, r *http.Request, ctx context.Context, objectKey, fileName, mimeType string) {
	stat, err := h.manager.StatObject(ctx, objectKey)
	if err != nil {
		h.failStream(w, r, err)
		return
	}
	if stat == nil || !stat.Exists {
		h.fail(w, r, http.StatusNotFound, fmt.Errorf("%w: object %q", fs.ErrNotExist, objectKey))
		return
	}
	if mimeType == "" {
		mimeType = stat.ContentType
	}
	h.writeContentHeaders(w, objectKey, fileName, mimeType, stat.Size)
}

// openForRequest 按请求头决定读整个对象还是读一个字节区间，并写好区间相关的响应头。
//
// Range 需要先知道对象大小才能换算（尤其是 "bytes=-500" 这种后缀区间），
// 因此只在确实带了 Range 头时才多付一次 Stat。
func (h *HTTPHandler) openForRequest(w http.ResponseWriter, r *http.Request, ctx context.Context, objectKey string) (io.ReadCloser, *model.ObjectStat, error) {
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		return h.manager.OpenObject(ctx, objectKey)
	}
	stat, err := h.manager.StatObject(ctx, objectKey)
	if err != nil {
		return nil, nil, err
	}
	if stat == nil || !stat.Exists {
		return nil, nil, fmt.Errorf("%w: object %q", fs.ErrNotExist, objectKey)
	}
	wanted, ok, err := parseByteRange(rangeHeader, stat.Size)
	if err != nil {
		// 唯一必须报错的情形：语法合法但区间整个落在对象之外。
		// 必须带上 Content-Range 告诉客户端真实长度，否则它无从纠正。
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(stat.Size, 10))
		return nil, nil, err
	}
	if !ok {
		return h.manager.OpenObject(ctx, objectKey)
	}
	reader, rangeStat, err := h.manager.OpenObjectRange(ctx, objectKey, wanted.offset, wanted.length)
	if errors.Is(err, manager.ErrRangeUnsupported) {
		// Provider 给不了区间读：HTTP 允许服务端忽略 Range，回吐整个对象，
		// 客户端照样能用，只是没省到流量。
		return h.manager.OpenObject(ctx, objectKey)
	}
	if err != nil {
		return nil, nil, err
	}
	size := statSize(rangeStat)
	if size <= 0 {
		size = stat.Size
	}
	w.Header().Set("Content-Range", wanted.contentRange(size))
	w.Header().Set("Content-Length", strconv.FormatInt(wanted.length, 10))
	return reader, rangeStat, nil
}

// failStream 映射读流阶段的错误。
func (h *HTTPHandler) failStream(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, manager.ErrProxyDownloadUnsupported):
		// 远端对象存储无法服务端读流，退化为 302 到底层签名地址。
		h.fail(w, r, http.StatusNotImplemented, err)
	case errors.Is(err, errRangeNotSatisfiable):
		h.fail(w, r, http.StatusRequestedRangeNotSatisfiable, err)
	default:
		h.fail(w, r, storeStatus(err), err)
	}
}

// writeContentHeaders 写内容相关的响应头并落下状态码。
//
// 区间响应的 Content-Length / Content-Range / 206 已经在 openForRequest 里写好，
// 这里不会覆盖它们：只有状态码仍是默认值时才按 200 处理。
func (h *HTTPHandler) writeContentHeaders(w http.ResponseWriter, objectKey, fileName, mimeType string, size int64) {
	if mimeType == "" {
		mimeType = mime.TypeByExtension(path.Ext(objectKey))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mimeType)
	// 声明支持断点续传。没有这个头，多数播放器与下载工具根本不会发 Range，
	// 实现了区间读也等于没实现。
	w.Header().Set("Accept-Ranges", "bytes")
	status := http.StatusOK
	if w.Header().Get("Content-Range") != "" {
		status = http.StatusPartialContent
	} else if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	// 回吐的是调用方上传的任意字节，Content-Type 也源自上传时的声明，
	// 而这个接口跑在业务主域上。没有下面这几个头，一次声明成 text/html
	// 的上传就等于在主域上挂了个可执行页面（存储型 XSS，可直接读同域 Cookie）。
	//
	// nosniff：禁止浏览器忽略 Content-Type 去嗅探内容；
	// CSP sandbox + default-src 'none'：即使真被当成文档渲染，脚本、表单、
	//   同源身份也全部失效；
	// attachment：除白名单内确定安全的类型，一律走下载而不是内联渲染。
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if fileName != "" {
		disposition := "attachment"
		if isInlineSafe(mimeType) {
			disposition = "inline"
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": fileName}))
	}
	// 受控资源不应被中间缓存留存
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
}

// statSize 取对象大小，stat 缺失时返回 0。
func statSize(stat *model.ObjectStat) int64 {
	if stat == nil {
		return 0
	}
	return stat.Size
}

// isInlineSafe 判断某个 MIME 类型能否安全地内联渲染。
//
// 白名单而不是黑名单：新出现的可执行格式默认走 attachment，
// 漏掉一种只是体验上多一次下载，而黑名单漏一种就是一个 XSS。
//
// image/svg+xml 必须排除——它虽然是 image/*，但可以内嵌 <script>，
// 是最常见的「伪装成图片的 XSS 载荷」。
func isInlineSafe(mimeType string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	if base == "image/svg+xml" || base == "image/svg" {
		return false
	}
	switch {
	case strings.HasPrefix(base, "image/"),
		strings.HasPrefix(base, "video/"),
		strings.HasPrefix(base, "audio/"):
		return true
	}
	return base == "application/pdf"
}

func signatureStatus(err error) int {
	switch {
	case errors.Is(err, provider.ErrSignatureExpired):
		return http.StatusGone
	// 地址绑定了用户但请求方没有身份：这是「请先登录」，不是「你不能看」，
	// 401 才能让客户端走补登录流程而不是直接放弃。
	case errors.Is(err, provider.ErrIdentityRequired):
		return http.StatusUnauthorized
	case errors.Is(err, provider.ErrIdentityMismatch):
		return http.StatusForbidden
	case errors.Is(err, provider.ErrSignatureMissing), errors.Is(err, provider.ErrSignatureInvalid):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}
