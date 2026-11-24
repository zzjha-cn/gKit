package integration_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// private 资源的签名地址绑定到签发时的用户：转发给别人立刻失效。
func TestPrivateSignedURLIsBoundToUser(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)
	res := uploadWithPolicy(t, svc, model.AccessPolicyPrivate, "alice")
	path, query := signedURLFor(t, svc, res, "alice")

	if query.Get(provider.SignParamBind) != "1" {
		t.Fatalf("private 签名地址缺少绑定标记 b=1，query = %v", query)
	}

	fetch := func(user string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
		if user != "" {
			req.Header.Set(transport.HeaderUserID, user)
		}
		return serve(router, req)
	}

	if rec := fetch("alice"); rec.Code != http.StatusOK {
		t.Fatalf("本人访问 status = %d，期望 200，body = %s", rec.Code, rec.Body.String())
	}
	// 把链接转发给别人
	if rec := fetch("bob"); rec.Code != http.StatusForbidden {
		t.Errorf("他人访问 status = %d，期望 403——private 签名地址可以转发使用", rec.Code)
	}
	// 没有身份的匿名请求：是「请先登录」而不是「你不能看」
	if rec := fetch(""); rec.Code != http.StatusUnauthorized {
		t.Errorf("匿名访问 status = %d，期望 401", rec.Code)
	}
}

// 写入侧的租户必须和读取侧同一个来源：取自表单的话，改个 account_id
// 就能把文件写进别的租户。
func TestUploadTenantComesFromHeaderNotForm(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)

	rec := uploadVia(t, router, map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-a",
		transport.HeaderUserID:    "alice",
	}, map[string]string{"biz_type": "doc"}, "header-identity")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	res := decodeUpload(t, rec)
	if res.Namespace != "crm" || res.AccountID != "acct-a" {
		t.Errorf("租户 = %s/%s，期望 crm/acct-a", res.Namespace, res.AccountID)
	}
	if res.Creator != "alice" {
		t.Errorf("creator = %q，期望 alice——创建者没有取自请求头，可以被冒充", res.Creator)
	}
}

// 表单里仍然带身份字段时：与请求头一致放行，不一致必须 400 而不是静默改写。
func TestUploadRejectsConflictingIdentityForm(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)
	headers := map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-a",
		transport.HeaderUserID:    "alice",
	}

	// 一致：放行，兼容还在同时传表单的老客户端
	rec := uploadVia(t, router, headers, map[string]string{
		"biz_type": "doc", "namespace": "crm", "account_id": "acct-a", "creator": "alice",
	}, "consistent")
	if rec.Code != http.StatusOK {
		t.Errorf("表单与请求头一致时 status = %d，期望 200，body = %s", rec.Code, rec.Body.String())
	}

	// 不一致：逐个字段都要拦住
	for field, value := range map[string]string{
		"account_id": "acct-b", "namespace": "other", "creator": "bob",
	} {
		rec = uploadVia(t, router, headers, map[string]string{"biz_type": "doc", field: value}, "conflict-"+field)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("表单 %s=%s 与请求头冲突时 status = %d，期望 400——"+
				"客户端以为写进了自己指定的租户，实际落在了别处", field, value, rec.Code)
		}
	}

	// 网关没注入头、客户端还在传表单：最危险的组合，同样算冲突
	rec = uploadVia(t, router, nil, map[string]string{"biz_type": "doc", "account_id": "acct-a"}, "no-header")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺请求头而表单有值时 status = %d，期望 400", rec.Code)
	}
}

// private 资源缺身份是「请先登录」，与签名路由的同类情形保持同一个状态码。
func TestPrivateWithoutIdentityIsUnauthorized(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
	})
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "note.txt", Reader: strings.NewReader("private"),
		Namespace: "crm", AccountID: "acct-a", Creator: "alice",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	router := newTestRouter(t, svc)
	req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+result.Resource.ID, nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	if rec := serve(router, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// B3：网关漏注入身份头时，读写会一起落到「空租户」——功能完全正常、
// 测试全绿，隔离却已经没了。require_identity 把这种静默失效换成一次 401。
func TestRequireIdentityFailsClosed(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Access.RequireIdentity = true
	})
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")
	router := newTestRouter(t, svc)

	bare := []struct {
		method string
		target string
	}{
		{http.MethodGet, transport.RouteGroup + "/list"},
		{http.MethodGet, transport.RouteGroup + "/" + owned.ID},
		{http.MethodGet, transport.RouteGroup + "/" + owned.ID + "/access"},
		{http.MethodGet, transport.RouteGroup + "/" + owned.ID + "/content"},
		{http.MethodDelete, transport.RouteGroup + "/" + owned.ID},
	}
	for _, tc := range bare {
		rec := serve(router, httptest.NewRequest(tc.method, tc.target, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 无身份头时 = %d，期望 401", tc.method, tc.target, rec.Code)
		}
	}

	// 上传同样 fail-closed，且必须是 401 而不是混在 400 里
	if rec := uploadVia(t, router, nil, map[string]string{"biz_type": "doc"}, "x"); rec.Code != http.StatusUnauthorized {
		t.Errorf("无身份头上传 = %d，期望 401", rec.Code)
	}

	// 带齐身份头照常工作
	req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+owned.ID, nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set(transport.HeaderUserID, "alice")
	if rec := serve(router, req); rec.Code != http.StatusOK {
		t.Errorf("带身份头 = %d，期望 200，body = %s", rec.Code, rec.Body.String())
	}

	// 开关默认关闭时保持既有行为（空租户可用），不影响存量接入方
	legacy := newTestService(t, nil)
	legacyRouter := newTestRouter(t, legacy)
	if rec := serve(legacyRouter, httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/list", nil)); rec.Code != http.StatusOK {
		t.Errorf("默认配置下无身份头的 list = %d，期望 200", rec.Code)
	}
}

// 接入方可以用自己的鉴权结果替换请求头方案：模块不关心身份从哪来。
func TestCustomIdentityResolver(t *testing.T) {
	svc := newTestService(t, nil)
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")

	// 一个完全不看请求头的解析器：身份来自 query 里的「会话 token」。
	// 真实接入方这里会是 auth.FromContext(c)，用 query 只是为了测试好构造。
	sessions := map[string]*model.AccessRequest{
		"tok-alice": {Namespace: "crm", AccountID: "acct-a", UserID: "alice"},
		"tok-bob":   {Namespace: "crm", AccountID: "acct-b", UserID: "bob"},
	}
	router := newRouterWithOptions(t, svc, transport.WithIdentityResolver(
		func(r *http.Request) (*model.AccessRequest, error) {
			token := r.URL.Query().Get("token")
			session, ok := sessions[token]
			if !ok {
				return nil, errors.New("unauthenticated session")
			}
			cloned := *session
			return &cloned, nil
		}))

	target := transport.RouteGroup + "/" + owned.ID
	// 属主的会话：正常读到
	if rec := serve(router, httptest.NewRequest(http.MethodGet, target+"?token=tok-alice", nil)); rec.Code != http.StatusOK {
		t.Errorf("属主会话 = %d，期望 200，body = %s", rec.Code, rec.Body.String())
	}
	// 别的租户的会话：仍然是 404，隔离逻辑不受身份来源影响
	if rec := serve(router, httptest.NewRequest(http.MethodGet, target+"?token=tok-bob", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("他租户会话 = %d，期望 404", rec.Code)
	}
	// 解析器报错：401
	if rec := serve(router, httptest.NewRequest(http.MethodGet, target+"?token=bad", nil)); rec.Code != http.StatusUnauthorized {
		t.Errorf("解析失败 = %d，期望 401", rec.Code)
	}
	// 挂了解析器之后，请求头不再被看一眼
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set(transport.HeaderUserID, "alice")
	if rec := serve(router, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("挂载解析器后请求头 = %d，期望 401（头已不参与解析）", rec.Code)
	}
}

// 写入侧必须和读取侧同一个来源：换了解析器，上传的落点也得跟着换。
func TestCustomIdentityResolverAppliesToUpload(t *testing.T) {
	svc := newTestService(t, nil)
	router := newRouterWithOptions(t, svc, transport.WithIdentityResolver(
		func(r *http.Request) (*model.AccessRequest, error) {
			return &model.AccessRequest{Namespace: "sso", AccountID: "org-7", UserID: "carol"}, nil
		}))

	// 请求头里塞的是另一套身份，必须被忽略
	rec := uploadVia(t, router, map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-evil",
		transport.HeaderUserID:    "mallory",
	}, map[string]string{"biz_type": "doc"}, "resolver-owns-identity")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeUpload(t, rec)
	if got.Namespace != "sso" || got.AccountID != "org-7" || got.Creator != "carol" {
		t.Errorf("落点 = %s/%s creator=%s，期望 sso/org-7 creator=carol",
			got.Namespace, got.AccountID, got.Creator)
	}

	// 表单与解析结果冲突仍然是 400，而不是 401
	rec = uploadVia(t, router, nil, map[string]string{"account_id": "org-9"}, "conflict")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("表单冲突 = %d，期望 400", rec.Code)
	}
}

// require_identity 对任何来源一视同仁：它检的是解析结果，不是请求头。
func TestRequireIdentityAppliesToCustomResolver(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) { cfg.Access.RequireIdentity = true })
	// 一个只认得出用户、认不出租户的解析器
	router := newRouterWithOptions(t, svc, transport.WithIdentityResolver(
		func(r *http.Request) (*model.AccessRequest, error) {
			return &model.AccessRequest{UserID: "dave"}, nil
		}))
	rec := serve(router, httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/list", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("租户为空时 = %d，期望 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "tenant identity is required") {
		t.Errorf("错误信息未指向租户缺失：%s", rec.Body.String())
	}
}

// 不挂载解析器时，行为与请求头方案完全一致（默认值不能退化成零隔离）。
func TestDefaultResolverStaysHeaderBased(t *testing.T) {
	svc := newTestService(t, nil)
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")
	router := newRouterWithOptions(t, svc) // 不传任何 Option

	req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+owned.ID, nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set(transport.HeaderUserID, "alice")
	if rec := serve(router, req); rec.Code != http.StatusOK {
		t.Errorf("默认解析器 + 正确请求头 = %d，期望 200", rec.Code)
	}
	// 传 nil 等价于不传
	nilOpt := newRouterWithOptions(t, svc, transport.WithIdentityResolver(nil))
	req2 := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+owned.ID, nil)
	req2.Header.Set(transport.HeaderNamespace, "crm")
	req2.Header.Set(transport.HeaderAccountID, "acct-a")
	req2.Header.Set(transport.HeaderUserID, "alice")
	if rec := serve(nilOpt, req2); rec.Code != http.StatusOK {
		t.Errorf("WithIdentityResolver(nil) = %d，期望 200", rec.Code)
	}
}

// 解析器返回 (nil, nil) 等价于匿名调用方：只能看到同样没有租户的资源，
// 而不是放行一切。
func TestNilIdentityIsAnonymousNotSuperuser(t *testing.T) {
	svc := newTestService(t, nil)
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")
	anonymous := upload(t, svc, "public.txt", []byte("no tenant")) // 没有租户字段

	router := newRouterWithOptions(t, svc, transport.WithIdentityResolver(
		func(r *http.Request) (*model.AccessRequest, error) { return nil, nil }))

	if rec := serve(router, httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+owned.ID, nil)); rec.Code != http.StatusNotFound {
		t.Errorf("匿名读他人租户资源 = %d，期望 404", rec.Code)
	}
	if rec := serve(router, httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+anonymous.Resource.ID, nil)); rec.Code != http.StatusOK {
		t.Errorf("匿名读无租户资源 = %d，期望 200", rec.Code)
	}
}

// 签名地址的 private 绑定校验也走解析器，不再硬编码读请求头。
func TestSignedRouteUsesResolverForBinding(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
	})
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "bound.txt", Reader: strings.NewReader("bound-content"),
		Namespace: "crm", AccountID: "acct-a", Creator: "alice",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	signed, err := url.Parse(result.URL)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}
	if signed.Query().Get(provider.SignParamBind) != "1" {
		t.Fatalf("private 资源的地址应当绑定用户：%s", result.URL)
	}

	// 解析器说请求者是 alice —— 放行；说是 mallory —— 403
	for _, tc := range []struct {
		user string
		want int
	}{{"alice", http.StatusOK}, {"mallory", http.StatusForbidden}} {
		user := tc.user
		router := newRouterWithOptions(t, svc, transport.WithIdentityResolver(
			func(r *http.Request) (*model.AccessRequest, error) {
				return &model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: user}, nil
			}))
		rec := serve(router, httptest.NewRequest(http.MethodGet, signed.Path+"?"+signed.RawQuery, nil))
		if rec.Code != tc.want {
			t.Errorf("请求者 %s = %d，期望 %d，body = %s", user, rec.Code, tc.want, rec.Body.String())
		}
	}
}
