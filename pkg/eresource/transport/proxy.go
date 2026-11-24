package transport

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"golang.org/x/sync/singleflight"
)

// 抓取中转相关的哨兵错误，供 proxyStatus 映射为 HTTP 状态码。
var (
	// ErrProxyDisabled 未启用抓取中转能力。
	ErrProxyDisabled = errors.New("eresource: resource proxy is disabled")
	// ErrProxyTargetBlocked 目标地址未通过 SSRF 校验。
	ErrProxyTargetBlocked = errors.New("eresource: proxy target is blocked")
	// ErrProxyTooLarge 远端资源体积超过 proxy.max_download_bytes。
	ErrProxyTooLarge = errors.New("eresource: remote resource exceeds the download size limit")
	// ErrProxyFetchFailed 远端抓取失败（连接、状态码、落盘等）。
	ErrProxyFetchFailed = errors.New("eresource: failed to fetch remote resource")
)

// tempFilePattern 下载中的临时文件前缀。清理任务据此跳过在途文件。
const tempFilePattern = ".downloading-"

// cleanupInterval 缓存清理轮询间隔。
const cleanupInterval = 6 * time.Hour

// resourceFetcher 负责把远端 URL 抓取到本地中转缓存。
//
// 所有状态都收敛在该结构体上，不使用包级变量：
// 包级可变状态既让一个进程只能有一份配置，与多实例装配冲突，也无法测试。
type resourceFetcher struct {
	cfg      config.ResourceProxyCfg
	client   *http.Client
	group    singleflight.Group
	stop     chan struct{}
	stopOnce sync.Once
}

// newResourceFetcher 构造抓取器并启动后台缓存清理任务。
//
// maxUploadBytes 是上传侧的体积上限（manager.max_file_size）。抓取上限会按它
// 收敛：抓下来的内容最终要走 Upload，一个 64MB 的远端文件在 32MB 的上传上限下
// 必然被 413 拒掉——先完整下完再落盘再拒绝，白烧一次带宽和一次磁盘写。
// 传 <=0 表示调用方没有上传侧约束，此时只用 proxy 自己的上限。
func newResourceFetcher(cfg config.ResourceProxyCfg, maxUploadBytes int64) (*resourceFetcher, error) {
	if cfg.CacheDir == "" {
		return nil, fmt.Errorf("%w: proxy.cache_dir is required", config.ErrInvalidConfig)
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("eresource: create proxy cache dir: %w", err)
	}
	if cfg.MaxDownloadSec <= 0 {
		cfg.MaxDownloadSec = config.DefaultProxyMaxDownloadSec
	}
	if cfg.MaxDownloadBytes <= 0 {
		cfg.MaxDownloadBytes = config.DefaultProxyMaxDownloadBytes
	}
	if maxUploadBytes > 0 {
		cfg.MaxDownloadBytes = min(cfg.MaxDownloadBytes, maxUploadBytes)
	}

	f := &resourceFetcher{cfg: cfg, stop: make(chan struct{})}

	transport := &http.Transport{}
	usingEgressProxy := false
	if cfg.ProxyURL != "" {
		proxyAddr, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("%w: proxy.proxy_url is not a valid url: %v", config.ErrInvalidConfig, err)
		}
		transport.Proxy = http.ProxyURL(proxyAddr)
		usingEgressProxy = true
	}
	// 在真正建连的那一刻校验对端 IP，这是唯一能挡住 DNS rebinding 的位置：
	// 域名白名单校验的是解析之前的名字，而这里拿到的是本次连接实际要用的地址。
	// 配了出口代理时跳过——此时连的是代理本身（很可能就在内网），出网管控应由该代理负责。
	if !usingEgressProxy {
		// 必须挂在 Dialer.Control 上：DialContext 收到的是 URL 里的原始主机名（尚未解析），
		// 而 Control 在解析之后、建连之前触发，拿到的才是本次连接真正要用的 IP。
		// 每个候选地址都会各调一次，因此多 A 记录里混入内网地址也会被逐个拦下。
		dialer := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control: func(network, address string, _ syscall.RawConn) error {
				return checkDialTarget(address)
			},
		}
		transport.DialContext = dialer.DialContext
	}

	f.client = &http.Client{
		Timeout:   time.Duration(cfg.MaxDownloadSec) * time.Second,
		Transport: transport,
		// 默认的 http.Client 会静默跟随 302，而跳转目标不会再经过入口处的白名单校验。
		// 攻击者只要用一个白名单内的域名跳到内网地址，白名单就形同虚设，
		// 所以每一跳都要重新过一遍同样的校验。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if err := checkTargetURL(cfg.AllowedHosts, req.URL); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			return nil
		},
	}

	go f.runCleanup()
	return f, nil
}

// Close 停止后台清理任务。重复调用是安全的。
func (f *resourceFetcher) Close() {
	if f == nil {
		return
	}
	f.stopOnce.Do(func() { close(f.stop) })
}

// fetchResult 一次抓取的结果。
type fetchResult struct {
	Path string // 本地缓存文件的绝对路径
	Ext  string // 从原始 URL 推断出的扩展名（带前导点）
	Name string // 原始文件名，取不到时为空
}

// fetch 校验目标地址并把远端资源抓取到本地缓存，返回缓存文件位置。
// 命中缓存时不会重新下载。
func (f *resourceFetcher) fetch(ctx context.Context, rawURL string) (*fetchResult, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProxyTargetBlocked, err)
	}
	if err = checkTargetURL(f.cfg.AllowedHosts, target); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProxyTargetBlocked, err)
	}

	// 用目标 URL 的 md5 做缓存键，避免原始路径里的非法字符与文件名碰撞。
	sum := md5.Sum([]byte(rawURL))
	cacheKey := hex.EncodeToString(sum[:])

	ext := strings.ToLower(filepath.Ext(target.Path))
	if ext == "" {
		ext = ".bin"
	}
	cachePath := filepath.Join(f.cfg.CacheDir, cacheKey+ext)

	// singleflight 防止多协程并发抓取同一个远端链接造成的瞬时带宽与连接数过载。
	_, err, _ = f.group.Do(cacheKey, func() (any, error) {
		if _, statErr := os.Stat(cachePath); statErr == nil {
			return nil, nil // 前序任务已下载完成
		}
		return nil, f.download(ctx, rawURL, cachePath)
	})
	if err != nil {
		return nil, err
	}

	result := &fetchResult{Path: cachePath, Ext: ext}
	if base := filepath.Base(target.Path); base != "" && base != "." && base != "/" && base != string(filepath.Separator) {
		result.Name = base
	}
	return result, nil
}

// download 拉取远端资源并原子性落盘。
//
// 先写入同目录下的唯一临时文件，fsync 后再 rename：
// rename 在同一文件系统内是原子的，因此其他协程要么看不到该文件，
// 要么看到的就是一个完整且已刷盘的文件，不会读到写了一半的内容。
func (f *resourceFetcher) download(ctx context.Context, rawURL, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProxyFetchFailed, err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProxyFetchFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: remote responded %s", ErrProxyFetchFailed, resp.Status)
	}
	maxBytes := f.cfg.MaxDownloadBytes
	// Content-Length 可信时先拒掉，省得白下一场。它可以缺失或撒谎，所以下面还要按实际字节数再卡一次。
	if resp.ContentLength > 0 && resp.ContentLength > maxBytes {
		return fmt.Errorf("%w: content-length %d exceeds limit %d", ErrProxyTooLarge, resp.ContentLength, maxBytes)
	}

	tmp, err := os.CreateTemp(f.cfg.CacheDir, tempFilePattern+"*")
	if err != nil {
		return fmt.Errorf("%w: create temp file: %v", ErrProxyFetchFailed, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// 多读 1 字节：只有真的超出上限时 written 才会等于 maxBytes+1。
	written, err := io.Copy(tmp, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProxyFetchFailed, err)
	}
	if written > maxBytes {
		err = fmt.Errorf("%w: exceeds limit %d bytes", ErrProxyTooLarge, maxBytes)
		return err
	}
	// Close 不保证数据落到物理介质，必须显式 Sync，否则掉电后 rename 出来的可能是个空洞文件。
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("%w: sync temp file: %v", ErrProxyFetchFailed, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("%w: close temp file: %v", ErrProxyFetchFailed, err)
	}
	if err = os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("%w: chmod temp file: %v", ErrProxyFetchFailed, err)
	}
	if err = os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("%w: rename temp file: %v", ErrProxyFetchFailed, err)
	}
	return nil
}

// runCleanup 周期性清理过期的缓存文件，直到 Close 被调用。
func (f *resourceFetcher) runCleanup() {
	if f.cfg.ExpireDays <= 0 {
		return
	}
	// 先立即清一次：否则进程重启频繁时，过期文件永远等不到第一次轮询。
	f.cleanupOnce()

	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-ticker.C:
			f.cleanupOnce()
		}
	}
}

func (f *resourceFetcher) cleanupOnce() {
	maxAge := time.Duration(f.cfg.ExpireDays) * 24 * time.Hour
	entries, err := os.ReadDir(f.cfg.CacheDir)
	if err != nil {
		slog.Error("Failed to read proxy cache dir", "dir", f.cfg.CacheDir, "err", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// 跳过在途的临时文件，否则会把正在下载的内容删掉。
		if strings.HasPrefix(entry.Name(), tempFilePattern) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		if time.Since(info.ModTime()) <= maxAge {
			continue
		}
		path := filepath.Join(f.cfg.CacheDir, entry.Name())
		if err = os.Remove(path); err == nil {
			slog.Info("Cleaned up expired proxy cached resource file", "path", path)
		}
	}
}

// checkTargetURL 校验一个待抓取的目标地址，入口请求与每一次重定向都要过这里。
func checkTargetURL(allowedHosts []string, target *url.URL) error {
	// 只允许 http/https。file://、gopher:// 之类的协议既没有意义，
	// 也是 SSRF 里常见的读本地文件 / 打内网协议的跳板。
	switch strings.ToLower(target.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("scheme %q is not allowed", target.Scheme)
	}
	host := target.Hostname()
	if host == "" {
		return fmt.Errorf("target host is empty")
	}
	if !isAllowedHost(allowedHosts, host) {
		return fmt.Errorf("host %q is not in the proxy whitelist", host)
	}
	// 直接写 IP 字面量的情况在这里就能拦掉，不必等到建连。
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("host %q resolves to a non-public address", host)
	}
	return nil
}

// isAllowedHost 判断主机名是否在白名单内。
//
// 传入的必须是 url.Hostname() 的结果（不含端口）：用 url.Host 比对时，
// 带端口的地址会让所有精确匹配失效。
func isAllowedHost(allowedHosts []string, host string) bool {
	if len(allowedHosts) == 0 {
		// 未配白名单时按放行处理。注意这依赖 checkDialTarget 的内网地址拦截兜底，
		// 生产环境仍建议显式配置白名单。
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, allowed := range allowedHosts {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == "" {
			continue
		}
		root := strings.TrimPrefix(allowed, ".")
		// 精确匹配主机名，或匹配其子域；绝不使用子串匹配，
		// 否则 example.com.evil.com 这类后缀伪装会被放行。
		if host == root || strings.HasSuffix(host, "."+root) {
			return true
		}
	}
	return false
}

// checkDialTarget 在建立 TCP 连接前校验对端地址，address 形如 "1.2.3.4:443"。
func checkDialTarget(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("invalid dial ip %q", host)
	}
	if !isPublicIP(ip) {
		return fmt.Errorf("refusing to connect to non-public address %s", ip)
	}
	return nil
}

// isPublicIP 判断地址是否属于可对外访问的公网地址。
// 回环、私有网段、链路本地（含 169.254.169.254 云元数据服务）等一律视为内网。
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// IPv4 保留段：100.64.0.0/10（CGNAT）与 192.0.0.0/24。
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return false
		}
		if v4[0] == 192 && v4[1] == 0 && v4[2] == 0 {
			return false
		}
		return true
	}
	// IPv6 唯一本地地址 fc00::/7。
	if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
		return false
	}
	return true
}

// proxyStatus 把抓取阶段的错误映射为 HTTP 状态码。
func proxyStatus(err error) int {
	switch {
	case errors.Is(err, ErrProxyDisabled):
		return http.StatusNotImplemented
	case errors.Is(err, ErrProxyTargetBlocked):
		return http.StatusForbidden
	case errors.Is(err, ErrProxyTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrProxyFetchFailed):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// ProxyUpload 抓取调用方指定的远端 URL，再按普通上传流程落库托管。
//
// 这是 Upload 的一个变体：区别只在于文件内容来自远端抓取而不是 multipart 表单，
// 抓取完成之后的校验、去重、落库与访问策略分流完全复用 Manager.Upload。
func (h *HTTPHandler) ProxyUpload(w http.ResponseWriter, r *http.Request) {
	if h.fetcher == nil {
		h.fail(w, r, proxyStatus(ErrProxyDisabled), ErrProxyDisabled)
		return
	}
	targetURL := h.formValue(r, "url")
	if targetURL == "" {
		targetURL = h.queryValue(r, "url")
	}
	if targetURL == "" {
		h.fail(w, r, http.StatusBadRequest, errors.New("missing 'url' parameter"))
		return
	}
	// 抓取中转同样会把远端内容落盘再交给 Upload，占用的是同一份资源预算，
	// 因此与普通上传共用一个并发闸门。
	if !h.acquireUploadSlot() {
		h.failBusy(w, r)
		return
	}
	defer h.releaseUploadSlot()

	// 抓取远端可能耗时数十秒，而 调用方的请求超时可能更短。
	// 直接复用请求 ctx 的话，下载还没结束 ctx 就过期了，后续落库必然失败，
	// 所以这里切断取消链并按 proxy.max_download_sec 重新计时。
	// 客户端断连不再中止抓取——已经下到一半的内容留在缓存里，下次请求可直接命中。
	fetchCtx, cancel := context.WithTimeout(
		context.WithoutCancel(r.Context()),
		time.Duration(h.cfg.Proxy.MaxDownloadSec)*time.Second,
	)
	defer cancel()

	fetched, err := h.fetcher.fetch(fetchCtx, targetURL)
	if err != nil {
		h.fail(w, r, proxyStatus(err), err)
		return
	}

	file, err := os.Open(fetched.Path)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, fmt.Errorf("open cached file: %w", err))
		return
	}
	defer file.Close()

	contentType := mime.TypeByExtension(fetched.Ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	fileName := fetched.Name
	if fileName == "" {
		fileName = filepath.Base(fetched.Path)
	}

	req := &model.UploadRequest{
		FileName:     fileName,
		ContentType:  contentType,
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

	result, err := h.manager.Upload(fetchCtx, req)
	if err != nil {
		h.fail(w, r, uploadStatus(err), err)
		return
	}
	h.ok(w, result.View())
}
