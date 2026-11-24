package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
)

func parseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// 白名单必须按主机名匹配：带端口时不能失效，后缀伪装不能放行。
func TestCheckTargetURLHostWhitelist(t *testing.T) {
	allowed := []string{"example.com", ".cdn.net"}

	ok := []string{
		"https://example.com/a.png",
		"https://example.com:8443/a.png", // 带端口不能让精确匹配失效
		"https://img.example.com/a.png",  // 子域
		"https://x.cdn.net/a.png",        // 配置带前导点
		"https://cdn.net/a.png",
	}
	for _, raw := range ok {
		if err := checkTargetURL(allowed, parseURL(t, raw)); err != nil {
			t.Errorf("expected %q to be allowed, got %v", raw, err)
		}
	}

	denied := []string{
		"https://example.com.evil.com/a.png", // 后缀伪装
		"https://evil.com/?redirect=example.com",
		"https://notexample.com/a.png",
		"https://evil.com/a.png",
	}
	for _, raw := range denied {
		if err := checkTargetURL(allowed, parseURL(t, raw)); err == nil {
			t.Errorf("expected %q to be denied", raw)
		}
	}
}

// 非 http/https 协议一律拒绝。
func TestCheckTargetURLRejectsNonHTTPScheme(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "gopher://127.0.0.1:11211/_x", "ftp://example.com/a"} {
		if err := checkTargetURL(nil, parseURL(t, raw)); err == nil {
			t.Errorf("expected scheme of %q to be rejected", raw)
		}
	}
}

// 白名单为空时默认放行主机名，但内网地址仍必须拦住——这是默认放行能成立的前提。
func TestCheckTargetURLBlocksInternalAddressesWithEmptyWhitelist(t *testing.T) {
	if err := checkTargetURL(nil, parseURL(t, "https://example.com/a")); err != nil {
		t.Fatalf("empty whitelist should allow public hosts, got %v", err)
	}
	denied := []string{
		"http://169.254.169.254/latest/meta-data/", // 云元数据服务
		"http://127.0.0.1:8080/admin",
		"http://10.1.2.3/",
		"http://192.168.0.1/",
		"http://[::1]/",
		"http://100.64.0.1/",
	}
	for _, raw := range denied {
		if err := checkTargetURL(nil, parseURL(t, raw)); err == nil {
			t.Errorf("expected %q to be denied", raw)
		}
	}
}

// 重定向目标必须复验，否则白名单内的域名一跳就能打进内网。
func TestRedirectTargetIsRevalidated(t *testing.T) {
	f := newTestFetcher(t, config.ResourceProxyCfg{AllowedHosts: []string{"example.com"}})
	if f.client.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set, otherwise redirects bypass the whitelist")
	}

	via := []*http.Request{{URL: parseURL(t, "https://example.com/a")}}
	blocked := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://evil.com/a",
		"http://127.0.0.1/",
	}
	for _, raw := range blocked {
		req := &http.Request{URL: parseURL(t, raw)}
		if err := f.client.CheckRedirect(req, via); err == nil {
			t.Errorf("expected redirect to %q to be blocked", raw)
		}
	}
	if err := f.client.CheckRedirect(&http.Request{URL: parseURL(t, "https://example.com/b")}, via); err != nil {
		t.Errorf("redirect within the whitelist should be allowed, got %v", err)
	}

	// 跳转次数也要有上限
	longVia := make([]*http.Request, 5)
	for i := range longVia {
		longVia[i] = via[0]
	}
	if err := f.client.CheckRedirect(&http.Request{URL: parseURL(t, "https://example.com/c")}, longVia); err == nil {
		t.Error("expected redirect chain to be capped")
	}
}

// checkDialTarget 是挡住 DNS rebinding 的最后一道：域名已通过白名单，但解析结果落在内网。
func TestCheckDialTargetRejectsInternalIP(t *testing.T) {
	for _, addr := range []string{"169.254.169.254:80", "127.0.0.1:8080", "10.0.0.1:443", "[fd00::1]:443"} {
		if err := checkDialTarget(addr); err == nil {
			t.Errorf("expected dial to %q to be refused", addr)
		}
	}
	for _, addr := range []string{"8.8.8.8:443", "[2001:4860:4860::8888]:443"} {
		if err := checkDialTarget(addr); err != nil {
			t.Errorf("expected dial to %q to be allowed, got %v", addr, err)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":              true,
		"2001:4860:4860::8888": true,
		"127.0.0.1":            false,
		"10.0.0.1":             false,
		"172.16.0.1":           false,
		"192.168.1.1":          false,
		"169.254.169.254":      false,
		"100.64.0.1":           false, // CGNAT
		"192.0.0.1":            false, // IETF 保留
		"0.0.0.0":              false,
		"::1":                  false,
		"fd00::1":              false, // IPv6 唯一本地地址
	}
	for raw, want := range cases {
		if got := isPublicIP(net.ParseIP(raw)); got != want {
			t.Errorf("isPublicIP(%s) = %t, want %t", raw, got, want)
		}
	}
}

// testURL 把 httptest 的 127.0.0.1 换成 localhost。
// checkTargetURL 会拒绝内网 IP 字面量（这是对的），而 localhost 不是 IP 字面量，
// 配合 srv.Client()（没有建连校验）就能在不放宽任何防线的前提下走通真实抓取路径。
func testURL(srv *httptest.Server, path string) string {
	return strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + path
}

func newTestFetcher(t *testing.T, cfg config.ResourceProxyCfg) *resourceFetcher {
	t.Helper()
	if cfg.CacheDir == "" {
		cfg.CacheDir = t.TempDir()
	}
	f, err := newResourceFetcher(cfg, 0)
	if err != nil {
		t.Fatalf("new fetcher: %v", err)
	}
	t.Cleanup(f.Close)
	return f
}

// 缓存目录为空必须报错：否则文件会散落在进程工作目录，既不可控也无法清理。
func TestFetcherRequiresCacheDir(t *testing.T) {
	if _, err := newResourceFetcher(config.ResourceProxyCfg{}, 0); err == nil {
		t.Fatal("expected an error when cache_dir is empty")
	} else if !errors.Is(err, config.ErrInvalidConfig) {
		t.Errorf("expected ErrInvalidConfig, got %v", err)
	}
}

// 超过 max_download_bytes 的远端资源必须拒绝，不能把中转盘打满。
func TestFetchRejectsOversizedBody(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/no-length" {
			// 不声明 Content-Length，强制走按实际字节数的兜底判断
			w.Header().Set("Transfer-Encoding", "chunked")
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := newTestFetcher(t, config.ResourceProxyCfg{MaxDownloadBytes: 1024})
	f.client = srv.Client() // 绕开内网地址拦截，httptest 监听在 127.0.0.1

	for _, path := range []string{"/with-length", "/no-length"} {
		if _, err := f.fetch(context.Background(), testURL(srv, path)); !errors.Is(err, ErrProxyTooLarge) {
			t.Errorf("%s: expected ErrProxyTooLarge, got %v", path, err)
		}
	}

	// 失败之后不能留下任何残留文件（包括写了一半的临时文件）
	entries, err := os.ReadDir(f.cfg.CacheDir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no leftover files after a rejected download, got %d", len(entries))
	}
}

// 正常抓取应落盘并可被复用；缓存命中时不再回源。
func TestFetchCachesAndReuses(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("remote-content"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, config.ResourceProxyCfg{MaxDownloadBytes: 1 << 20})
	f.client = srv.Client()

	first, err := f.fetch(context.Background(), testURL(srv, "/a.png"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if first.Ext != ".png" || first.Name != "a.png" {
		t.Errorf("unexpected fetch result %+v", first)
	}
	data, err := os.ReadFile(first.Path)
	if err != nil || string(data) != "remote-content" {
		t.Fatalf("cached content = %q, err = %v", data, err)
	}

	second, err := f.fetch(context.Background(), testURL(srv, "/a.png"))
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if second.Path != first.Path {
		t.Errorf("expected the cache to be reused, got %q vs %q", second.Path, first.Path)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected exactly 1 origin hit, got %d", got)
	}
}

// 非 200 响应不应留下缓存文件，后续请求要能重试。
func TestFetchDoesNotCacheFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	f := newTestFetcher(t, config.ResourceProxyCfg{MaxDownloadBytes: 1 << 20})
	f.client = srv.Client()

	if _, err := f.fetch(context.Background(), testURL(srv, "/missing.png")); !errors.Is(err, ErrProxyFetchFailed) {
		t.Fatalf("expected ErrProxyFetchFailed, got %v", err)
	}
	entries, _ := os.ReadDir(f.cfg.CacheDir)
	if len(entries) != 0 {
		t.Errorf("a failed fetch must not leave cache files, got %d", len(entries))
	}
}

// 清理任务不能删掉正在下载的临时文件。
func TestCleanupSkipsInFlightTempFiles(t *testing.T) {
	dir := t.TempDir()
	f := newTestFetcher(t, config.ResourceProxyCfg{CacheDir: dir, ExpireDays: 1})

	stale := filepath.Join(dir, "stale.bin")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	inFlight := filepath.Join(dir, tempFilePattern+"123")
	if err := os.WriteFile(inFlight, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(inFlight, old, old); err != nil {
		t.Fatal(err)
	}

	f.cleanupOnce()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("expected the expired cache file to be removed")
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("an in-flight temp file must not be cleaned up: %v", err)
	}
}

// 抓取阶段的错误必须映射成对应的 HTTP 状态码，而不是一律 200/500。
func TestProxyStatusMapping(t *testing.T) {
	cases := map[error]int{
		ErrProxyDisabled:      http.StatusNotImplemented,
		ErrProxyTargetBlocked: http.StatusForbidden,
		ErrProxyTooLarge:      http.StatusRequestEntityTooLarge,
		ErrProxyFetchFailed:   http.StatusBadGateway,
		errors.New("other"):   http.StatusInternalServerError,
	}
	for err, want := range cases {
		if got := proxyStatus(fmt.Errorf("wrapped: %w", err)); got != want {
			t.Errorf("proxyStatus(%v) = %d, want %d", err, got, want)
		}
	}
}

// 直连模式下，内网拦截必须发生在 DNS 解析之后。
//
// 如果把校验挂在 Transport.DialContext 上，拿到的是 URL 里的原始主机名，
// 任何域名都会被当成「非法 IP」拒绝——防线看着在，实际是所有抓取全挂。
// 这里用 localhost（解析到 127.0.0.1）确认：报的是解析后的地址不合法，
// 而不是主机名本身不合法。
func TestDialGuardRunsAfterDNSResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not be reachable"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, config.ResourceProxyCfg{MaxDownloadBytes: 1 << 20})

	_, err := f.fetch(context.Background(), testURL(srv, "/a.png"))
	if err == nil {
		t.Fatal("expected the dial guard to block a loopback target")
	}
	if strings.Contains(err.Error(), "invalid dial ip") {
		t.Fatalf("guard rejected the hostname instead of the resolved address: %v", err)
	}
	if !strings.Contains(err.Error(), "non-public address") {
		t.Errorf("expected a resolved-address rejection, got %v", err)
	}
}

// 抓取上限必须按上传上限收敛：抓下来的内容最终要走 Upload，
// 一个 64MB 的远端文件在 32MB 的上传上限下必然被 413 拒掉——
// 先完整下完再落盘再拒绝，白烧一次带宽和一次磁盘写。
func TestFetcherClampsDownloadLimitToUploadLimit(t *testing.T) {
	cases := map[string]struct {
		download, upload, want int64
	}{
		"上传上限更小":  {64 << 20, 32 << 20, 32 << 20},
		"抓取上限更小":  {8 << 20, 32 << 20, 8 << 20},
		"没有上传侧约束": {8 << 20, 0, 8 << 20},
		"抓取未配置":   {0, 16 << 20, 16 << 20},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, err := newResourceFetcher(config.ResourceProxyCfg{
				CacheDir:         t.TempDir(),
				MaxDownloadBytes: tc.download,
			}, tc.upload)
			if err != nil {
				t.Fatalf("new fetcher: %v", err)
			}
			t.Cleanup(f.Close)
			if f.cfg.MaxDownloadBytes != tc.want {
				t.Errorf("max_download_bytes = %d, want %d", f.cfg.MaxDownloadBytes, tc.want)
			}
		})
	}
}
