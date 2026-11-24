package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

func newTestService(t *testing.T, tune func(*config.Config)) *eresource.Service {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Manager: config.ManagerConfig{
			DefaultAccessPolicy: model.AccessPolicyPublic,
			MaxFileSize:         1024,
		},
		Store:    config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(dir, "meta.json")},
		Provider: config.ProviderConfig{Type: provider.TypeLocal, RootDir: filepath.Join(dir, "objects"), Domain: "cdn.test", Scheme: "https"},
		Access:   config.AccessConfig{SignedURLSecret: "unit-test-secret", SignedURLDefaultExpire: 60},
	}
	if tune != nil {
		tune(&cfg)
	}
	svc, err := eresource.New(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func upload(t *testing.T, svc *eresource.Service, name string, body []byte) *model.UploadResult {
	t.Helper()
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: name,
		Reader:   bytes.NewReader(body),
		BizType:  "image",
	})
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	return result
}

// newTestRouter 装配一套注册好全部路由的 HTTP 接入层。
func newTestRouter(t *testing.T, svc *eresource.Service) http.Handler {
	t.Helper()
	handler, err := transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	t.Cleanup(handler.Close)
	router := http.NewServeMux()
	handler.Register(router)
	return router
}

func serve(router http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("content of %s = %q, want %q", path, got, want)
	}
}

// pngBytes 返回一个最小的合法 PNG，用于让内容嗅探识别为 image/png。
func pngBytes() []byte {
	return []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
	}
}

// uploadForTenant 以指定租户身份上传一份资源。
func uploadForTenant(t *testing.T, svc *eresource.Service, ns, account, creator string) *model.Resource {
	t.Helper()
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "note.txt", Reader: strings.NewReader("tenant-" + ns + "-" + account),
		Namespace: ns, AccountID: account, BizType: "doc",
		AccessPolicy: model.AccessPolicyProtected, Creator: creator,
	})
	if err != nil {
		t.Fatalf("upload for tenant %s/%s: %v", ns, account, err)
	}
	return result.Resource
}

func uploadWithPolicy(t *testing.T, svc *eresource.Service, policy, creator string) *model.Resource {
	t.Helper()
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "note.txt", Reader: strings.NewReader("bind-" + policy),
		Namespace: "crm", AccountID: "acct-a", BizType: "doc",
		AccessPolicy: policy, Creator: creator,
	})
	if err != nil {
		t.Fatalf("upload %s: %v", policy, err)
	}
	return result.Resource
}

// uploadVia 以请求头携带身份发起一次 multipart 上传，form 里的额外字段按需叠加。
func uploadVia(t *testing.T, router http.Handler, headers, form map[string]string, content string) *httptest.ResponseRecorder {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(transport.FormFieldFile, "note.txt")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write([]byte(content)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	for k, v := range form {
		if err = writer.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, transport.RouteGroup+"/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return serve(router, req)
}

func decodeUpload(t *testing.T, rec *httptest.ResponseRecorder) *model.Resource {
	t.Helper()
	var resp struct {
		Data struct {
			Resource *model.Resource `json:"resource"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload response: %v, body = %s", err, rec.Body.String())
	}
	return resp.Data.Resource
}

// newRouterWithOptions 与 newTestRouter 相同，但允许传入 transport.Option。
func newRouterWithOptions(t *testing.T, svc *eresource.Service, opts ...transport.Option) http.Handler {
	t.Helper()
	handler, err := transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config, opts...)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	t.Cleanup(handler.Close)
	router := http.NewServeMux()
	handler.Register(router)
	return router
}

// baseConfig 复刻 newTestService 的配置，但不建服务——
// 需要用同一份元数据文件起第二个实例的用例要自己控制生命周期。
func baseConfig(dir string) config.Config {
	return config.Config{
		Manager: config.ManagerConfig{
			DefaultAccessPolicy: model.AccessPolicyPublic,
			MaxFileSize:         1024,
		},
		Store:    config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(dir, "meta.json")},
		Provider: config.ProviderConfig{Type: provider.TypeLocal, RootDir: filepath.Join(dir, "objects"), Domain: "cdn.test", Scheme: "https"},
		Access:   config.AccessConfig{SignedURLSecret: "unit-test-secret", SignedURLDefaultExpire: 60},
	}
}

func readMetaFile(t *testing.T, svc *eresource.Service) string {
	t.Helper()
	raw, err := os.ReadFile(svc.Config.Store.JSONFile)
	if err != nil {
		t.Fatalf("read metadata file: %v", err)
	}
	return string(raw)
}

func strPtr(s string) *string { return &s }

// serviceWithRoot 在 Service 之外额外记住存储根目录——
// 孤儿对账的用例要直接往底层目录里塞文件、改时间戳。
type serviceWithRoot struct {
	*eresource.Service
	root string
}

func newServiceWithRoot(t *testing.T, tune func(*config.Config), metrics ...manager.Metrics) *serviceWithRoot {
	t.Helper()
	dir := t.TempDir()
	cfg := baseConfig(dir)
	if tune != nil {
		tune(&cfg)
	}
	var opts []eresource.Option
	for _, m := range metrics {
		opts = append(opts, eresource.WithMetrics(m))
	}
	svc, err := eresource.New(cfg, opts...)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	return &serviceWithRoot{Service: svc, root: filepath.Join(dir, "objects")}
}

func uploadAs(t *testing.T, svc *eresource.Service, name string, body []byte, creator string) *model.UploadResult {
	t.Helper()
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: name,
		Reader:   bytes.NewReader(body),
		BizType:  "video",
		Creator:  creator,
	})
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	return result
}
