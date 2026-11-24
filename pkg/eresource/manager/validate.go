package manager

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// DefaultMaxFileSize 未配置 MaxFileSize 时使用的兜底上限。
// 上传体积必须始终有上限，否则单个超大请求就能把整个进程的内存打爆。
const DefaultMaxFileSize int64 = 32 << 20 // 32MB

var (
	ErrEmptyUpload      = errors.New("eresource: upload input is empty")
	ErrFileTooLarge     = errors.New("eresource: file exceeds max allowed size")
	ErrExtNotAllowed    = errors.New("eresource: file extension is not allowed")
	ErrMimeNotAllowed   = errors.New("eresource: mime type is not allowed")
	ErrMimeMismatch     = errors.New("eresource: declared mime type does not match file content")
	ErrResourceInactive = errors.New("eresource: resource status is invalid")
	// ErrUnsafePublicContent 该内容类型不允许以 public 策略托管。
	ErrUnsafePublicContent = errors.New("eresource: content type is not allowed for the public access policy")
)

// unsafePublicMimeTypes 不允许以 public 策略托管的内容类型。
//
// public 资源的直链指向 CDN / OSS 域或静态目录，/content 那套
// nosniff + CSP sandbox + attachment 防护在这条路径上完全不经过：
// 浏览器会直接按 Content-Type 渲染，等于在托管域上挂了一个可执行页面。
// 这里的类型都能携带并执行脚本（SVG 可内嵌 <script>，XML 可挂 XSLT）。
var unsafePublicMimeTypes = map[string]bool{
	"text/html":             true,
	"application/xhtml+xml": true,
	"image/svg+xml":         true,
	"image/svg":             true,
	"text/xml":              true,
	"application/xml":       true,
}

// unsafePublicExts 不允许以 public 策略托管的扩展名。
//
// 必须连扩展名一起拦：local + nginx、以及 OSS 在 Content-Type 缺失时，
// 都是按扩展名决定响应头的，只看声明类型会被 application/octet-stream
// 加 .html 的组合绕过。
var unsafePublicExts = map[string]bool{
	".html": true, ".htm": true, ".xhtml": true, ".shtml": true,
	".svg": true, ".svgz": true, ".xml": true, ".xsl": true, ".xslt": true,
}

// validatePublicContent 拦截以 public 策略上传的可执行文档类型。
//
// 三个维度都查：调用方声明的类型、扩展名、内容嗅探结果。
// 只查声明类型的话，声明成 application/octet-stream 再带个 .html 后缀即可绕过；
// 只查扩展名的话，无后缀 + 声明 text/html 同样能过。
//
// 需要托管 SVG 图标这类内容时，改用 protected / private 走 /content 回吐
// （那条路径有 CSP 与 attachment 兜底），或显式打开 allow_public_active_content。
func (m *DefaultManager) validatePublicContent(accessPolicy, declared, ext string, body []byte) error {
	if accessPolicy != model.AccessPolicyPublic || m.config.Manager.AllowPublicActiveContent {
		return nil
	}
	if unsafePublicExts[strings.ToLower(ext)] {
		return fmt.Errorf("%w: ext=%q", ErrUnsafePublicContent, ext)
	}
	for _, mimeType := range []string{declared, sniffedMimeType(body)} {
		base := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
		if unsafePublicMimeTypes[base] {
			return fmt.Errorf("%w: mime=%q", ErrUnsafePublicContent, base)
		}
	}
	return nil
}

// maxFileSize 返回生效的上传体积上限。
func (m *DefaultManager) maxFileSize() int64 {
	if m.config.Manager.MaxFileSize > 0 {
		return m.config.Manager.MaxFileSize
	}
	return DefaultMaxFileSize
}

// readUploadBody 在体积上限内读取上传内容。
// 多读 1 字节用于区分“刚好等于上限”与“超出上限”。
func (m *DefaultManager) readUploadBody(req *model.UploadRequest) ([]byte, error) {
	max := m.maxFileSize()
	if req.Reader != nil {
		body, err := io.ReadAll(io.LimitReader(req.Reader, max+1))
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > max {
			return nil, fmt.Errorf("%w: limit=%d", ErrFileTooLarge, max)
		}
		if len(body) == 0 {
			return nil, ErrEmptyUpload
		}
		return body, nil
	}
	if len(req.Bytes) == 0 {
		return nil, ErrEmptyUpload
	}
	if int64(len(req.Bytes)) > max {
		return nil, fmt.Errorf("%w: limit=%d", ErrFileTooLarge, max)
	}
	return req.Bytes, nil
}

// resolveExt 归一化扩展名，返回值统一为小写且带前导点。
func resolveExt(fileName string) string {
	return strings.ToLower(filepath.Ext(fileName))
}

// resolveMimeType 解析生效的 MIME 类型：调用方声明优先，缺省时按内容嗅探。
func resolveMimeType(declared string, body []byte) string {
	if declared = strings.TrimSpace(declared); declared != "" {
		return declared
	}
	sniffLen := len(body)
	if sniffLen > 512 {
		sniffLen = 512
	}
	return http.DetectContentType(body[:sniffLen])
}

// sniffedMimeType 按内容嗅探 MIME 类型。
func sniffedMimeType(body []byte) string {
	sniffLen := len(body)
	if sniffLen > 512 {
		sniffLen = 512
	}
	return http.DetectContentType(body[:sniffLen])
}

// mediaTopLevel 取 MIME 的顶级类型，如 "image/png; q=1" -> "image"。
func mediaTopLevel(mimeType string) string {
	base := strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0])
	top, _, _ := strings.Cut(base, "/")
	return strings.ToLower(top)
}

// validateMimeConsistency 校验声明类型与嗅探类型是否自洽。
//
// 拦截的是把 HTML / 脚本伪装成图片上传、再通过公开 URL 触发存储型 XSS 这一类攻击。
// 只对 image/video/audio 生效：docx、csv 这类格式嗅探结果本就与声明不同，不参与校验。
// 嗅探为 application/octet-stream 时说明 Go 认不出该格式（如 heic），放行以免误伤。
func (m *DefaultManager) validateMimeConsistency(declared string, body []byte) error {
	if m.config.Manager.SkipMimeSniffCheck || declared == "" {
		return nil
	}
	declaredTop := mediaTopLevel(declared)
	switch declaredTop {
	case "image", "video", "audio":
	default:
		return nil
	}
	detected := sniffedMimeType(body)
	if strings.HasPrefix(detected, "application/octet-stream") {
		return nil
	}
	if mediaTopLevel(detected) != declaredTop {
		return fmt.Errorf("%w: declared=%q detected=%q", ErrMimeMismatch, declared, detected)
	}
	return nil
}

// validateUpload 执行扩展名与 MIME 白名单校验。白名单为空表示不限制。
func (m *DefaultManager) validateUpload(ext, mimeType string) error {
	if allowed := m.config.Manager.AllowedExts; len(allowed) > 0 {
		if !containsFold(allowed, ext, normalizeExtEntry) {
			return fmt.Errorf("%w: ext=%q", ErrExtNotAllowed, ext)
		}
	}
	if allowed := m.config.Manager.AllowedMimeTypes; len(allowed) > 0 {
		// 去掉 "; charset=utf-8" 之类的参数后再比对
		base := strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0])
		if !containsFold(allowed, base, strings.TrimSpace) {
			return fmt.Errorf("%w: mime=%q", ErrMimeNotAllowed, base)
		}
	}
	return nil
}

// normalizeExtEntry 让白名单同时接受 "png" 与 ".png" 两种写法。
func normalizeExtEntry(entry string) string {
	entry = strings.TrimSpace(entry)
	if entry == "" || strings.HasPrefix(entry, ".") {
		return entry
	}
	return "." + entry
}

func containsFold(list []string, target string, normalize func(string) string) bool {
	for _, item := range list {
		if strings.EqualFold(normalize(item), target) {
			return true
		}
	}
	return false
}
