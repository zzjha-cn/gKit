package integration_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/local"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
	"github.com/zzjha-cn/gKit/pkg/eresource/store/jsonstore"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// 修复点 5：Referer 必须按主机名匹配，不能用子串。
func TestRefererWhitelistMatchesHostNotSubstring(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPublic
		cfg.Access.EnableHotlinkProtection = true
		cfg.Access.RefererWhitelist = []string{"example.com", "*.cdn.test"}
	})
	result := upload(t, svc, "img.txt", []byte("pixels"))
	id := result.Resource.ID

	allowed := []string{
		"https://example.com/page",
		"http://example.com:8080/a?x=1",
		"https://img.cdn.test/x",
		"https://cdn.test/x",
	}
	for _, referer := range allowed {
		if err := svc.Manager.VerifyAccess(context.Background(), id, &model.AccessRequest{Referer: referer}); err != nil {
			t.Errorf("referer %q should be allowed, got %v", referer, err)
		}
	}

	denied := []string{
		"https://evil.com/?redirect=example.com", // 子串匹配会误放行
		"https://example.com.evil.com/x",         // 后缀伪装
		"https://notexample.com/x",
		"https://evil.test/x",
		"not-a-url",
	}
	for _, referer := range denied {
		err := svc.Manager.VerifyAccess(context.Background(), id, &model.AccessRequest{Referer: referer})
		if !errors.Is(err, manager.ErrHotlinkDenied) {
			t.Errorf("referer %q should be denied, got %v", referer, err)
		}
	}

	// 空 Referer 默认拒绝，可通过配置放行
	if err := svc.Manager.VerifyAccess(context.Background(), id, &model.AccessRequest{}); !errors.Is(err, manager.ErrHotlinkDenied) {
		t.Errorf("empty referer: err = %v, want ErrHotlinkDenied", err)
	}
}

// 底层对象被清理后，代理下载应当返回 404 而不是 500。
func TestContentReturnsNotFoundWhenObjectMissing(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "gone.txt", []byte("to-be-removed"))

	// 绕过 Manager 直接删掉物理文件，制造元数据还在、对象没了的状态
	if err := os.Remove(result.Resource.StorePath); err != nil {
		t.Fatalf("remove object: %v", err)
	}

	handler, err := transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	t.Cleanup(handler.Close)
	router := http.NewServeMux()
	handler.Register(router)

	req := httptest.NewRequest(http.MethodGet, "/resource/"+result.Resource.ID+"/content", nil)
	if rec := serve(router, req); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the underlying object is gone", rec.Code)
	}
}

// 受控下载跑在业务主域上，必须确保上传的内容无法在该域执行脚本。
func TestDownloadHeadersPreventScriptExecution(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		// 关掉嗅探一致性校验，模拟「业务确实允许上传 HTML」的最坏情况
		cfg.Manager.SkipMimeSniffCheck = true
	})
	router := newTestRouter(t, svc)

	cases := []struct {
		name        string
		fileName    string
		contentType string
		body        string
		disposition string
	}{
		{"html payload", "evil.html", "text/html", "<script>alert(document.cookie)</script>", "attachment"},
		{"svg payload", "evil.svg", "image/svg+xml", "<svg xmlns=\"http://www.w3.org/2000/svg\"><script/></svg>", "attachment"},
		{"real image", "ok.png", "image/png", "\x89PNG\r\n\x1a\nfake", "inline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
				FileName: tc.fileName, ContentType: tc.contentType,
				Reader: strings.NewReader(tc.body), BizType: "doc",
				AccessPolicy: model.AccessPolicyProtected, Creator: "alice",
			})
			if err != nil {
				t.Fatalf("upload: %v", err)
			}
			req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+result.Resource.ID+"/content", nil)
			req.Header.Set("X-User-Id", "alice")
			rec := serve(router, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("content status = %d, body = %s", rec.Code, rec.Body.String())
			}
			header := rec.Result().Header
			if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
				t.Errorf("Content-Security-Policy = %q, want a sandbox directive", got)
			}
			if got := header.Get("Content-Disposition"); !strings.HasPrefix(got, tc.disposition) {
				t.Errorf("Content-Disposition = %q, want %s", got, tc.disposition)
			}
		})
	}
}

// 回吐对象内容不能受 HTTP 框架单请求超时约束，否则大文件会被静默截断。
func TestDownloadOutlivesRequestTimeout(t *testing.T) {
	dir := t.TempDir()
	local, err := local.NewLocalProvider(config.ProviderConfig{
		RootDir: filepath.Join(dir, "objects"), Domain: "cdn.test", Scheme: "https",
	})
	if err != nil {
		t.Fatalf("new local provider: %v", err)
	}
	stub := &stubProvider{Provider: local, readDelay: 120 * time.Millisecond}

	svc, err := eresource.New(config.Config{
		Store:    config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(dir, "meta.json")},
		Provider: config.ProviderConfig{Type: provider.TypeLocal, RootDir: filepath.Join(dir, "objects"), Domain: "cdn.test", Scheme: "https"},
		Manager:  config.ManagerConfig{DefaultAccessPolicy: model.AccessPolicyProtected},
		Access:   config.AccessConfig{SignedURLSecret: "test-secret"},
	}, eresource.WithProvider(stub))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)

	payload := "download me"
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "note.txt", Reader: strings.NewReader(payload),
		BizType: "doc", Creator: "alice",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	handler, err := transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	t.Cleanup(handler.Close)
	// 把请求上下文的期限压到远小于读取耗时。
	router := http.NewServeMux()
	handler.Register(router)

	req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+result.Resource.ID+"/content", nil)
	req.Header.Set("X-User-Id", "alice")
	ctx, cancel := context.WithTimeout(req.Context(), 20*time.Millisecond)
	defer cancel()
	rec := serve(router, req.WithContext(ctx))

	if rec.Code != http.StatusOK {
		t.Fatalf("content status = %d, body = %s", rec.Code, rec.Body.String())
	}
	stub.mu.Lock()
	readCtxErr := stub.readCtxErr
	stub.mu.Unlock()
	if readCtxErr != nil {
		t.Errorf("read context was cancelled mid-download: %v; 下载 ctx 必须脱离单请求超时", readCtxErr)
	}
	if got := rec.Body.String(); got != payload {
		t.Errorf("body = %q, want %q; 内容被截断说明下载沿用了请求超时", got, payload)
	}
}

// T4：存储故障不能被渲染成 403。
// 渲染成 403 的后果是一次 Mongo 故障在监控上表现为一片「权限拒绝」：
// 既不触发 5xx 告警，客户端也不会重试。
func TestStorageFailureIsNotRenderedAsForbidden(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		Manager:  config.ManagerConfig{DefaultAccessPolicy: model.AccessPolicyPublic},
		Provider: config.ProviderConfig{Type: provider.TypeLocal, RootDir: dir, Domain: "cdn.test", Scheme: "https"},
		Access:   config.AccessConfig{SignedURLSecret: "unit-test-secret"},
	}
	svc, err := eresource.New(cfg, eresource.WithStore(brokenStore{err: errors.New("mongo: server selection timeout")}))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	router := newTestRouter(t, svc)

	for _, route := range []string{"", "/access", "/content"} {
		target := transport.RouteGroup + "/abc" + route
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set(transport.HeaderNamespace, "crm")
		req.Header.Set(transport.HeaderAccountID, "acct-a")
		req.Header.Set(transport.HeaderUserID, "alice")
		if rec := serve(router, req); rec.Code != http.StatusInternalServerError {
			t.Errorf("GET %s 在存储故障时 = %d，期望 500", target, rec.Code)
		}
	}
}

// T13：受控下载原先走 Get + VerifyAccess，每次下载查两次库。
func TestContentQueriesTheStoreOnce(t *testing.T) {
	svc, counting := newCountingService(t)
	resource := uploadOwned(t, svc, nil)
	router := newTestRouter(t, svc)

	counting.getByID.Store(0)
	req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+resource.ID+"/content", nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set(transport.HeaderUserID, "alice")
	rec := serve(router, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := counting.getByID.Load(); got != 1 {
		t.Errorf("GetByID called %d times, want 1", got)
	}
}

func TestPrivateLargeFileRedirects(t *testing.T) {
	body := bytes.Repeat([]byte("v"), 600)
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
		c.Access.PrivateRedirectMinBytes = 500
	})
	result := uploadAs(t, svc, "movie.bin", body, "u-1")

	router := newTestRouter(t, svc)
	req := httptest.NewRequest(http.MethodGet, "/resource/"+result.Resource.ID+"/content", nil)
	req.Header.Set("X-User-Id", "u-1")
	rec := serve(router, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	location := rec.Header().Get("Location")
	if location == "" {
		t.Fatal("302 without a Location header")
	}
	if !strings.Contains(location, result.Resource.ObjectKey) {
		t.Errorf("redirect %q does not point at the object", location)
	}
}

// 小于阈值的照旧走代理读流：分流只针对真正烧带宽的大文件。
func TestPrivateSmallFileStillProxies(t *testing.T) {
	body := bytes.Repeat([]byte("v"), 100)
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
		c.Access.PrivateRedirectMinBytes = 500
	})
	result := uploadAs(t, svc, "thumb.bin", body, "u-1")

	router := newTestRouter(t, svc)
	req := httptest.NewRequest(http.MethodGet, "/resource/"+result.Resource.ID+"/content", nil)
	req.Header.Set("X-User-Id", "u-1")
	rec := serve(router, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Error("proxied body does not match the original")
	}
}

// 分流不是鉴权旁路：跨租户请求在跳转之前就该被挡掉。
func TestRedirectDoesNotBypassAuthorization(t *testing.T) {
	body := bytes.Repeat([]byte("v"), 600)
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
		c.Access.PrivateRedirectMinBytes = 500
	})
	ctx := context.Background()
	result, err := svc.Manager.Upload(ctx, &model.UploadRequest{
		FileName:  "movie.bin",
		Reader:    bytes.NewReader(body),
		BizType:   "video",
		Namespace: "crm",
		AccountID: "acct-a",
		Creator:   "u-1",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	router := newTestRouter(t, svc)
	req := httptest.NewRequest(http.MethodGet, "/resource/"+result.Resource.ID+"/content", nil)
	req.Header.Set("X-Resource-Namespace", "crm")
	req.Header.Set("X-Resource-Account-Id", "acct-b")
	req.Header.Set("X-User-Id", "u-9")
	rec := serve(router, req)
	if rec.Code == http.StatusFound {
		t.Fatal("a cross-tenant request was redirected to a signed url")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// brokenStore 是一个必然失败的 Store，用来观察存储故障在 HTTP 上的表现。
type brokenStore struct{ err error }

func (s brokenStore) Create(context.Context, *model.Resource) error { return s.err }

func (s brokenStore) Update(context.Context, *model.Resource) error { return s.err }

func (s brokenStore) Patch(context.Context, string, *model.ResourcePatch) error {
	return s.err
}

func (s brokenStore) Delete(context.Context, string) error { return s.err }

func (s brokenStore) GetByID(context.Context, string) (*model.Resource, error) {
	return nil, s.err
}

func (s brokenStore) GetByHash(context.Context, string) (*model.Resource, error) {
	return nil, s.err
}

func (s brokenStore) List(context.Context, *model.Query) ([]*model.Resource, error) {
	return nil, s.err
}

func (s brokenStore) Count(context.Context, *model.Query) (int64, error) { return 0, s.err }

// countingStore 统计 GetByID 的调用次数，用来确认下载路径不再重复查库。
type countingStore struct {
	store.Store
	getByID atomic.Int64
}

func (s *countingStore) GetByID(ctx context.Context, id string) (*model.Resource, error) {
	s.getByID.Add(1)
	return s.Store.GetByID(ctx, id)
}

// newCountingService 装配一个带查询计数的服务。
func newCountingService(t *testing.T) (*eresource.Service, *countingStore) {
	t.Helper()
	dir := t.TempDir()
	inner, err := jsonstore.NewJSONStore(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatalf("new json store: %v", err)
	}
	counting := &countingStore{Store: inner}
	svc, err := eresource.New(config.Config{
		Manager: config.ManagerConfig{DefaultAccessPolicy: model.AccessPolicyPrivate, MaxFileSize: 1024},
		Store:   config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(dir, "meta.json")},
		Provider: config.ProviderConfig{
			Type: provider.TypeLocal, RootDir: filepath.Join(dir, "objects"),
			Domain: "cdn.test", Scheme: "https",
		},
		Access: config.AccessConfig{SignedURLSecret: "unit-test-secret", SignedURLDefaultExpire: 60},
	}, eresource.WithStore(counting))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc, counting
}
