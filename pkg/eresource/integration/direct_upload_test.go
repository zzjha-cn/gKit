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
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// 凭证必须把 object key、体积上限与票据一并签出：
// 这三样是服务端在直传路径上唯一还能施加的约束。
func TestDirectUploadPolicyIsIssued(t *testing.T) {
	svc, _ := newDirectService(t, nil)
	policy, err := directManager(t, svc).CreateDirectUpload(context.Background(),
		directRequest("crm", "acct-a", "alice", "photo.png", "image/png"))
	if err != nil {
		t.Fatalf("create direct upload: %v", err)
	}
	if policy.ObjectKey == "" || policy.Ticket == "" {
		t.Fatalf("policy = %+v，object_key 与 ticket 都不能为空", policy)
	}
	if policy.MaxSize != 1024 {
		t.Errorf("max_size = %d, want 1024——体积上限必须写进凭证，否则一张凭证能灌任意大的文件", policy.MaxSize)
	}
	if !policy.ExpireAt.After(time.Now()) {
		t.Errorf("expire_at = %v 已过期", policy.ExpireAt)
	}
	// Bucket / StorePath 是服务端内部字段，不能序列化给客户端。
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if strings.Contains(string(raw), "mem-bucket/mem/direct") {
		t.Errorf("凭证里泄露了底层存储路径: %s", raw)
	}
}

// 直传完成后必须回来登记，否则对象查不到也不会被回收。
func TestDirectUploadCompleteRegistersResource(t *testing.T) {
	svc, p := newDirectService(t, nil)
	uploader := directManager(t, svc)
	policy, err := uploader.CreateDirectUpload(context.Background(),
		directRequest("crm", "acct-a", "alice", "photo.png", "image/png"))
	if err != nil {
		t.Fatalf("create direct upload: %v", err)
	}
	p.putDirect(policy.ObjectKey, []byte("direct-bytes"))

	result, err := uploader.CompleteDirectUpload(context.Background(), &model.DirectUploadCompleteRequest{
		Ticket: policy.Ticket, Namespace: "crm", AccountID: "acct-a", Creator: "alice",
		Tags: []string{"banner"},
	})
	if err != nil {
		t.Fatalf("complete direct upload: %v", err)
	}
	res := result.Resource
	if res.Namespace != "crm" || res.AccountID != "acct-a" || res.Creator != "alice" {
		t.Errorf("租户与归属取自票据，got %s/%s/%s", res.Namespace, res.AccountID, res.Creator)
	}
	// 大小以 Stat 为准，不采信客户端上报。
	if res.Size != int64(len("direct-bytes")) {
		t.Errorf("size = %d, want %d——必须以 provider.Stat 为准", res.Size, len("direct-bytes"))
	}
	if res.Hash != "" {
		t.Errorf("hash = %q，服务端没读过内容就不该有摘要", res.Hash)
	}
	// 登记之后必须能按普通资源读出来。
	got, err := svc.Manager.Get(context.Background(), res.ID,
		&model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "alice"})
	if err != nil {
		t.Fatalf("get registered resource: %v", err)
	}
	if got.ObjectKey != policy.ObjectKey {
		t.Errorf("object_key = %q, want %q", got.ObjectKey, policy.ObjectKey)
	}
}

// 票据是自包含的，被改写之后必须验不过——否则任何人都能把任意
// object key 登记成自己租户下的资源。
func TestDirectUploadRejectsForgedTicket(t *testing.T) {
	svc, p := newDirectService(t, nil)
	uploader := directManager(t, svc)
	policy, err := uploader.CreateDirectUpload(context.Background(),
		directRequest("crm", "acct-a", "alice", "photo.png", "image/png"))
	if err != nil {
		t.Fatalf("create direct upload: %v", err)
	}
	p.putDirect(policy.ObjectKey, []byte("direct-bytes"))

	payload, signature, _ := strings.Cut(policy.Ticket, ".")
	cases := map[string]string{
		"载荷被改写": payload[:len(payload)-2] + "AA" + "." + signature,
		"签名被改写": payload + "." + strings.Repeat("0", len(signature)),
		"没有签名":  payload,
		"空票据":   "",
	}
	for name, ticket := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := uploader.CompleteDirectUpload(context.Background(), &model.DirectUploadCompleteRequest{
				Ticket: ticket, Namespace: "crm", AccountID: "acct-a", Creator: "alice",
			})
			if !errors.Is(err, manager.ErrDirectUploadTicketInvalid) {
				t.Fatalf("err = %v, want ErrDirectUploadTicketInvalid", err)
			}
		})
	}
}

// 票据是不记名字符串，泄露出去就是一次可用的写入许可：
// 登记时必须再比对一次调用方身份。
func TestDirectUploadTicketIsBoundToCaller(t *testing.T) {
	svc, p := newDirectService(t, nil)
	uploader := directManager(t, svc)
	policy, err := uploader.CreateDirectUpload(context.Background(),
		directRequest("crm", "acct-a", "alice", "photo.png", "image/png"))
	if err != nil {
		t.Fatalf("create direct upload: %v", err)
	}
	p.putDirect(policy.ObjectKey, []byte("direct-bytes"))

	thieves := map[string]*model.DirectUploadCompleteRequest{
		"别的租户":   {Ticket: policy.Ticket, Namespace: "crm", AccountID: "acct-b", Creator: "alice"},
		"别的命名空间": {Ticket: policy.Ticket, Namespace: "im", AccountID: "acct-a", Creator: "alice"},
		"别的用户":   {Ticket: policy.Ticket, Namespace: "crm", AccountID: "acct-a", Creator: "mallory"},
		"匿名":     {Ticket: policy.Ticket},
	}
	for name, req := range thieves {
		t.Run(name, func(t *testing.T) {
			if _, err := uploader.CompleteDirectUpload(context.Background(), req); !errors.Is(err, manager.ErrDirectUploadIdentityMismatch) {
				t.Fatalf("err = %v, want ErrDirectUploadIdentityMismatch", err)
			}
		})
	}
}

// 拿了凭证却没真传：不能凭票据登记出一条指向空对象的记录。
func TestDirectUploadRequiresTheObjectToExist(t *testing.T) {
	svc, _ := newDirectService(t, nil)
	uploader := directManager(t, svc)
	policy, err := uploader.CreateDirectUpload(context.Background(),
		directRequest("crm", "acct-a", "alice", "photo.png", "image/png"))
	if err != nil {
		t.Fatalf("create direct upload: %v", err)
	}
	_, err = uploader.CompleteDirectUpload(context.Background(), &model.DirectUploadCompleteRequest{
		Ticket: policy.Ticket, Namespace: "crm", AccountID: "acct-a", Creator: "alice",
	})
	if !errors.Is(err, manager.ErrDirectUploadObjectMissing) {
		t.Fatalf("err = %v, want ErrDirectUploadObjectMissing", err)
	}
}

// 直传看不到内容，但声明值这一层的拦截不能少：
// public + HTML/SVG 这个组合在直传路径上同样要拒。
func TestDirectUploadKeepsPublicContentGuard(t *testing.T) {
	svc, _ := newDirectService(t, nil)
	uploader := directManager(t, svc)
	for _, tc := range []struct{ name, fileName, contentType string }{
		{"声明 html", "page.bin", "text/html"},
		{"html 扩展名", "page.html", "application/octet-stream"},
		{"svg 扩展名", "icon.svg", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := directRequest("crm", "acct-a", "alice", tc.fileName, tc.contentType)
			req.AccessPolicy = model.AccessPolicyPublic
			if _, err := uploader.CreateDirectUpload(context.Background(), req); !errors.Is(err, manager.ErrUnsafePublicContent) {
				t.Fatalf("err = %v, want ErrUnsafePublicContent", err)
			}
		})
	}
}

// 默认不开直传；local provider 即使开了也给不出凭证。
func TestDirectUploadIsOffByDefault(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
	})
	_, err := directManager(t, svc).CreateDirectUpload(context.Background(),
		directRequest("crm", "acct-a", "alice", "photo.png", "image/png"))
	if !errors.Is(err, manager.ErrDirectUploadDisabled) {
		t.Fatalf("err = %v, want ErrDirectUploadDisabled", err)
	}

	local := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
		cfg.Manager.EnableDirectUpload = true
	})
	_, err = directManager(t, local).CreateDirectUpload(context.Background(),
		directRequest("crm", "acct-a", "alice", "photo.png", "image/png"))
	if !errors.Is(err, manager.ErrDirectUploadUnsupported) {
		t.Fatalf("err = %v, want ErrDirectUploadUnsupported——local 盘没有客户端可直连的端点", err)
	}
}

// 开了直传但没配签名密钥时必须在启动阶段就报错：
// 没有密钥，票据就是人人可伪造的明文。
func TestDirectUploadRequiresSignedURLSecret(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		Manager: config.ManagerConfig{DefaultAccessPolicy: model.AccessPolicyPublic, EnableDirectUpload: true},
		Store:   config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(dir, "meta.json")},
		Provider: config.ProviderConfig{
			Type: provider.TypeLocal, RootDir: filepath.Join(dir, "objects"), Domain: "cdn.test", Scheme: "https",
		},
	}
	cfg.Normalize()
	if err := cfg.Validate(); err == nil {
		t.Fatal("缺少 signed_url_secret 时配置校验通过了，期望报错")
	}
}

// 直传的两个接口要能从 HTTP 侧跑通，且身份只认 IdentityResolver。
func TestDirectUploadOverHTTP(t *testing.T) {
	svc, p := newDirectService(t, nil)
	router := newTestRouter(t, svc)
	headers := map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-a",
		transport.HeaderUserID:    "alice",
	}

	policyReq := httptest.NewRequest(http.MethodPost,
		transport.RouteGroup+"/upload/policy?biz_type=image&file_name=photo.png&content_type=image/png", nil)
	for k, v := range headers {
		policyReq.Header.Set(k, v)
	}
	rec := serve(router, policyReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var policyResp struct {
		Data *model.DirectUploadPolicy `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &policyResp); err != nil {
		t.Fatalf("decode policy: %v, body = %s", err, rec.Body.String())
	}
	policy := policyResp.Data
	if policy == nil || policy.Ticket == "" {
		t.Fatalf("policy 响应缺少票据: %s", rec.Body.String())
	}
	p.putDirect(policy.ObjectKey, []byte("direct-over-http"))

	completeReq := httptest.NewRequest(http.MethodPost,
		transport.RouteGroup+"/upload/complete?ticket="+url.QueryEscape(policy.Ticket), nil)
	for k, v := range headers {
		completeReq.Header.Set(k, v)
	}
	if rec = serve(router, completeReq); rec.Code != http.StatusOK {
		t.Fatalf("complete status = %d, body = %s", rec.Code, rec.Body.String())
	}
	res := decodeUpload(t, rec)
	if res.Size != int64(len("direct-over-http")) {
		t.Errorf("size = %d, want %d", res.Size, len("direct-over-http"))
	}

	// 换一个租户拿同一张票据来登记：403，不能靠持票写进别人的空间。
	stolen := httptest.NewRequest(http.MethodPost,
		transport.RouteGroup+"/upload/complete?ticket="+url.QueryEscape(policy.Ticket), nil)
	stolen.Header.Set(transport.HeaderNamespace, "crm")
	stolen.Header.Set(transport.HeaderAccountID, "acct-b")
	stolen.Header.Set(transport.HeaderUserID, "mallory")
	if rec = serve(router, stolen); rec.Code != http.StatusForbidden {
		t.Fatalf("stolen ticket status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
}

// 没开直传时路由根本不注册：留一个永远 501 的端点只会浪费接入方的排查时间。
func TestDirectUploadRoutesAreNotRegisteredWhenDisabled(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)
	rec := serve(router, httptest.NewRequest(http.MethodPost, transport.RouteGroup+"/upload/policy", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404——未开启直传时不应注册该路由", rec.Code)
	}
}

// newDirectService 装配一个开启了直传、且 provider 支持直传的服务。
func newDirectService(t *testing.T, tune func(*config.Config)) (*eresource.Service, *memProvider) {
	t.Helper()
	p := newMemProvider()
	dir := t.TempDir()
	cfg := config.Config{
		Manager: config.ManagerConfig{
			DefaultAccessPolicy: model.AccessPolicyPrivate,
			MaxFileSize:         1024,
			EnableDirectUpload:  true,
		},
		Store:    config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(dir, "meta.json")},
		Provider: config.ProviderConfig{Type: provider.TypeLocal, RootDir: filepath.Join(dir, "objects"), Domain: "cdn.test", Scheme: "https"},
		Access:   config.AccessConfig{SignedURLSecret: "unit-test-secret", SignedURLDefaultExpire: 60},
	}
	if tune != nil {
		tune(&cfg)
	}
	svc, err := eresource.New(cfg, eresource.WithProvider(p))
	if err != nil {
		t.Fatalf("new direct service: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc, p
}

func directManager(t *testing.T, svc *eresource.Service) manager.DirectUploader {
	t.Helper()
	uploader, ok := svc.Manager.(manager.DirectUploader)
	if !ok {
		t.Fatalf("manager %T 没有实现 DirectUploader", svc.Manager)
	}
	return uploader
}

func directRequest(ns, account, creator, fileName, contentType string) *model.DirectUploadRequest {
	return &model.DirectUploadRequest{
		Namespace: ns, AccountID: account, Creator: creator,
		BizType: "image", FileName: fileName, ContentType: contentType,
	}
}
