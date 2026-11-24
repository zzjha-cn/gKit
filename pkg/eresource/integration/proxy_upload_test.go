package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// 抓取中转作为 HTTP 接入层的附属能力：启用后路由随 Handler 一并注册，
// 抓回来的内容照常走 Manager.Upload 的校验与落库流程。
func TestProxyUploadEndToEnd(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes())
	}))
	defer origin.Close()

	cacheDir := t.TempDir()
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Proxy.Enable = true
		cfg.Proxy.CacheDir = cacheDir
		cfg.Proxy.MaxDownloadBytes = 1 << 20
		// 走出口代理模式：此时建连的是代理本身，直连模式下的内网地址拦截不适用，
		// 测试因此可以指向监听在 127.0.0.1 的 httptest 服务而不必放宽任何防线。
		cfg.Proxy.ProxyURL = origin.URL
		cfg.Proxy.AllowedHosts = []string{"cdn.example.com"}
	})
	handler, err := transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	t.Cleanup(handler.Close)
	router := http.NewServeMux()
	handler.Register(router)

	target := "http://cdn.example.com/logo.png"
	form := url.Values{"url": {target}, "biz_type": {"image"}}
	req := httptest.NewRequest(http.MethodPost, "/resource/proxy-upload", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-1")

	rec := serve(router, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy-upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Resource *model.Resource `json:"resource"`
		} `json:"data"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body = %s", err, rec.Body.String())
	}
	res := resp.Data.Resource
	if res == nil {
		t.Fatalf("no resource in response: %s", rec.Body.String())
	}
	if res.Name != "logo.png" || res.MimeType != "image/png" {
		t.Errorf("unexpected resource %+v", res)
	}
	// 响应里不再带 object_key（存储布局不外泄），租户前缀改从服务端的记录上核对。
	if res.ObjectKey != "" || res.StorePath != "" || res.Bucket != "" {
		t.Errorf("response leaks storage layout: %+v", res)
	}
	stored, err := svc.Manager.Get(context.Background(), res.ID, nil)
	if err != nil {
		t.Fatalf("get stored resource: %v", err)
	}
	if !strings.HasPrefix(stored.ObjectKey, "crm/acct-1/image/") {
		t.Errorf("proxy upload should honour tenant scoping, got %q", stored.ObjectKey)
	}

	// 白名单外的地址必须被拦住，且返回 403 而不是 200
	blocked := url.Values{"url": {"http://169.254.169.254/latest/meta-data/"}}
	req = httptest.NewRequest(http.MethodPost, "/resource/proxy-upload", strings.NewReader(blocked.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec = serve(router, req); rec.Code != http.StatusForbidden {
		t.Errorf("blocked target status = %d, want 403", rec.Code)
	}

	// 缺参数返回 400
	req = httptest.NewRequest(http.MethodPost, "/resource/proxy-upload", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec = serve(router, req); rec.Code != http.StatusBadRequest {
		t.Errorf("missing url status = %d, want 400", rec.Code)
	}
}

// 未启用时不注册路由，也不应有后台清理任务。
func TestProxyUploadRouteAbsentWhenDisabled(t *testing.T) {
	svc := newTestService(t, nil)
	handler, err := transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	t.Cleanup(handler.Close)
	router := http.NewServeMux()
	handler.Register(router)

	req := httptest.NewRequest(http.MethodPost, "/resource/proxy-upload", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := serve(router, req); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the proxy is disabled", rec.Code)
	}
}

// 启用抓取中转但没配缓存目录，必须在启动阶段就报错。
func TestProxyRequiresCacheDir(t *testing.T) {
	cfg := config.Config{
		Manager:  config.ManagerConfig{DefaultAccessPolicy: model.AccessPolicyPublic},
		Store:    config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(t.TempDir(), "meta.json")},
		Provider: config.ProviderConfig{Type: provider.TypeLocal, RootDir: t.TempDir(), Scheme: "https"},
		Proxy:    config.ResourceProxyCfg{Enable: true},
	}
	if _, err := eresource.New(cfg); !errors.Is(err, config.ErrInvalidConfig) {
		t.Errorf("expected ErrInvalidConfig when proxy is enabled without cache_dir, got %v", err)
	}
}
