package integration_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// P0-1：去重范围必须卡在租户边界上。
// Namespace / AccountID 一旦漏掉，B 租户上传同样内容时会拿到 A 租户的 Resource
// （连同它的 ID、Creator、Metadata），属于越权。
func TestDedupDoesNotCrossTenant(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.EnableDedup = true
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPublic
	})
	body := []byte("tenant-shared-content")
	req := func(ns, account string) *model.UploadRequest {
		return &model.UploadRequest{
			FileName: "f.txt", Reader: bytes.NewReader(body),
			BizType: "avatar", AccessPolicy: model.AccessPolicyPublic, Creator: "alice",
			Namespace: ns, AccountID: account,
		}
	}
	mustUpload := func(r *model.UploadRequest) *model.UploadResult {
		t.Helper()
		result, err := svc.Manager.Upload(context.Background(), r)
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		return result
	}

	first := mustUpload(req("crm", "acct-1"))

	// 同租户同维度：应当命中去重
	if same := mustUpload(req("crm", "acct-1")); !same.Existed || same.Resource.ObjectKey != first.Resource.ObjectKey {
		t.Errorf("same tenant should hit dedup, existed=%t", same.Existed)
	}

	// 换账户：不得复用
	if got := mustUpload(req("crm", "acct-2")); got.Existed {
		t.Error("dedup must not cross account_id")
	}
	// 换命名空间：不得复用
	if got := mustUpload(req("im", "acct-1")); got.Existed {
		t.Error("dedup must not cross namespace")
	}
	// 不带租户信息：Store 把空串当「不过滤」，这里必须靠显式边界核对兜住
	if got := mustUpload(req("", "")); got.Existed {
		t.Error("an untenanted upload must not reuse a tenant-owned resource")
	}
}

// 租户信息应当体现在物理路径上，避免不同租户的对象混在同一层目录。
func TestObjectKeyIsTenantScoped(t *testing.T) {
	svc := newTestService(t, nil)
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "a.png", Reader: bytes.NewReader(pngBytes()),
		BizType: "avatar", Namespace: "crm", AccountID: "acct-1",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !strings.HasPrefix(result.Resource.ObjectKey, "crm/acct-1/avatar/") {
		t.Errorf("object key %q should be prefixed by namespace/account/biz_type", result.Resource.ObjectKey)
	}
	if result.Resource.Namespace != "crm" || result.Resource.AccountID != "acct-1" {
		t.Errorf("tenant fields not persisted: %+v", result.Resource)
	}
}

// 修复点 15：身份字段非法时必须明确报错，不能静默剥离。
//
// 静默剥离会让 account "a.b" 与 "ab" 折叠成同一个路径段，
// 两个租户的对象落进同一层目录，隔离边界在无人察觉时消失。
func TestInvalidTenantSegmentIsRejected(t *testing.T) {
	svc := newTestService(t, nil)

	for _, req := range []*model.UploadRequest{
		{FileName: "a.png", BizType: "image", AccountID: "a.b"},
		{FileName: "a.png", BizType: "image", Namespace: "crm/../im"},
		{FileName: "a.png", BizType: "img type"},
		{FileName: "a.png", BizType: "image", AccountID: strings.Repeat("x", 65)},
	} {
		req.Reader = bytes.NewReader(pngBytes())
		_, err := svc.Manager.Upload(context.Background(), req)
		if !errors.Is(err, provider.ErrInvalidSegment) {
			t.Errorf("ns=%q account=%q biz=%q: err = %v, want ErrInvalidSegment",
				req.Namespace, req.AccountID, req.BizType, err)
		}
	}

	// 合法输入照常通过
	if _, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "a.png", Reader: bytes.NewReader(pngBytes()), BizType: "image", AccountID: "a-b",
	}); err != nil {
		t.Errorf("valid segment rejected: %v", err)
	}
}

// 资源 ID 是全局的，光靠 object key 的租户前缀挡不住任何东西：
// 读取侧必须自己校验租户，否则拿到 ID 就等于拿到内容。
func TestCrossTenantReadIsRejected(t *testing.T) {
	svc := newTestService(t, nil)
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")

	insiders := map[string]*model.AccessRequest{
		"same tenant": {Namespace: "crm", AccountID: "acct-a", UserID: "alice"},
	}
	outsiders := map[string]*model.AccessRequest{
		"other account":   {Namespace: "crm", AccountID: "acct-b", UserID: "mallory"},
		"other namespace": {Namespace: "im", AccountID: "acct-a", UserID: "mallory"},
		// 不带租户信息不能成为权限最大的调用方式
		"no tenant at all": {UserID: "mallory"},
	}

	for name, req := range insiders {
		if _, err := svc.Manager.Get(context.Background(), owned.ID, req); err != nil {
			t.Errorf("%s: get = %v, want success", name, err)
		}
		if err := svc.Manager.VerifyAccess(context.Background(), owned.ID, req); err != nil {
			t.Errorf("%s: verify access = %v, want success", name, err)
		}
		if _, err := svc.Manager.GetAccessURL(context.Background(), owned.ID, req); err != nil {
			t.Errorf("%s: access url = %v, want success", name, err)
		}
	}

	for name, req := range outsiders {
		if _, err := svc.Manager.Get(context.Background(), owned.ID, req); !errors.Is(err, manager.ErrResourceNotVisible) {
			t.Errorf("%s: get = %v, want ErrResourceNotVisible", name, err)
		}
		if err := svc.Manager.VerifyAccess(context.Background(), owned.ID, req); !errors.Is(err, manager.ErrResourceNotVisible) {
			t.Errorf("%s: verify access = %v, want ErrResourceNotVisible", name, err)
		}
		if _, err := svc.Manager.GetAccessURL(context.Background(), owned.ID, req); !errors.Is(err, manager.ErrResourceNotVisible) {
			t.Errorf("%s: access url = %v, want ErrResourceNotVisible", name, err)
		}
		if err := svc.Manager.Delete(context.Background(), owned.ID, req); !errors.Is(err, manager.ErrResourceNotVisible) {
			t.Errorf("%s: delete = %v, want ErrResourceNotVisible", name, err)
		}
	}
}

// 跨租户一律按「不存在」回应：返回 403 等于确认该 ID 真实存在。
func TestCrossTenantOverHTTPLooksLikeNotFound(t *testing.T) {
	svc := newTestService(t, nil)
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")
	router := newTestRouter(t, svc)

	withHeaders := func(method, target, ns, account, user string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set(transport.HeaderNamespace, ns)
		req.Header.Set(transport.HeaderAccountID, account)
		req.Header.Set("X-User-Id", user)
		return serve(router, req)
	}

	for _, route := range []string{"", "/access", "/content"} {
		target := transport.RouteGroup + "/" + owned.ID + route
		if rec := withHeaders(http.MethodGet, target, "crm", "acct-b", "mallory"); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s as other tenant = %d, want 404", target, rec.Code)
		}
		if rec := withHeaders(http.MethodGet, target, "crm", "acct-a", "alice"); rec.Code != http.StatusOK {
			t.Errorf("GET %s as owner = %d, want 200", target, rec.Code)
		}
	}

	// 同租户非上传者的删除是 403：这个信息不泄露任何跨租户的存在性
	if rec := withHeaders(http.MethodDelete, transport.RouteGroup+"/"+owned.ID, "crm", "acct-a", "bob"); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE as non-creator = %d, want 403", rec.Code)
	}
	if rec := withHeaders(http.MethodDelete, transport.RouteGroup+"/"+owned.ID, "crm", "acct-b", "mallory"); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE as other tenant = %d, want 404", rec.Code)
	}
}

// ScopedQuery 必须是「覆盖」而不是「为空时填充」。
//
// 这条容易在后续迭代里被悄悄改坏：只要 listQuery 哪天开始读
// ?account_id=，「为空时填充」就会立刻退化成客户端自选范围。
// 因此直接对这个函数下断言，而不是依赖上层恰好没传该参数。
func TestScopedQueryOverridesCallerSuppliedTenant(t *testing.T) {
	forged := &model.Query{Namespace: "im", AccountID: "acct-b", BizType: "doc"}
	caller := &model.AccessRequest{Namespace: "crm", AccountID: "acct-a"}

	scoped := manager.ScopedQuery(forged, caller)
	if scoped.Namespace != "crm" || scoped.AccountID != "acct-a" {
		t.Errorf("scoped tenant = %q/%q, want crm/acct-a; caller-supplied values must be overwritten",
			scoped.Namespace, scoped.AccountID)
	}
	// 非租户条件不受影响
	if scoped.BizType != "doc" {
		t.Errorf("biz_type = %q, want doc", scoped.BizType)
	}

	// 调用方没有租户身份时，范围也必须收窄到「无租户」，而不是保留原值
	anonymous := manager.ScopedQuery(&model.Query{Namespace: "im", AccountID: "acct-b"}, &model.AccessRequest{})
	if anonymous.Namespace != "" || anonymous.AccountID != "" {
		t.Errorf("anonymous scope = %q/%q, want empty/empty", anonymous.Namespace, anonymous.AccountID)
	}

	// nil query 不能 panic
	if got := manager.ScopedQuery(nil, caller); got == nil || got.AccountID != "acct-a" {
		t.Errorf("ScopedQuery(nil, caller) = %+v", got)
	}
}

// 开了去重时，写入侧的租户隔离直接决定了会不会变成跨租户的存在性探测接口：
// 指定目标租户上传一份已知内容，命中去重就能拿到那条记录的完整元数据。
func TestUploadCannotProbeOtherTenantViaDedup(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.EnableDedup = true
	})
	router := newTestRouter(t, svc)
	const secret = "victim-owned-content"

	// 受害者先上传
	rec := uploadVia(t, router, map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-victim",
		transport.HeaderUserID:    "victim",
	}, map[string]string{"biz_type": "doc"}, secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("victim upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	victim := decodeUpload(t, rec)

	// 攻击者拿着同样的内容，试图在表单里指定受害者的租户
	rec = uploadVia(t, router, map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-attacker",
		transport.HeaderUserID:    "mallory",
	}, map[string]string{"biz_type": "doc", "account_id": "acct-victim"}, secret)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("表单指定他人租户时 status = %d，期望 400", rec.Code)
	}

	// 不带表单字段时，落点必须是攻击者自己的租户，且不能命中受害者的记录
	rec = uploadVia(t, router, map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-attacker",
		transport.HeaderUserID:    "mallory",
	}, map[string]string{"biz_type": "doc"}, secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("attacker upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeUpload(t, rec)
	if got.ID == victim.ID {
		t.Errorf("去重跨越了租户边界：攻击者拿到了受害者的资源记录 %s", victim.ID)
	}
	if got.AccountID != "acct-attacker" {
		t.Errorf("落点租户 = %q，期望 acct-attacker", got.AccountID)
	}
}

// B17：删除权限需要管理员一档。缺了它，allow_cross_creator_delete
// 关着则运营代删 / 离职清理全部卡死，开着则同租户任意用户都能删别人的文件。
func TestAdminMayDeleteOthersResource(t *testing.T) {
	// 未打开 trust_admin_header 时，这个头不被信任
	strict := newTestService(t, nil)
	owned := uploadForTenant(t, strict, "crm", "acct-a", "alice")
	strictRouter := newTestRouter(t, strict)

	adminReq := func(router http.Handler, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, transport.RouteGroup+"/"+id, nil)
		req.Header.Set(transport.HeaderNamespace, "crm")
		req.Header.Set(transport.HeaderAccountID, "acct-a")
		req.Header.Set(transport.HeaderUserID, "ops")
		req.Header.Set(transport.HeaderIsAdmin, "1")
		return serve(router, req)
	}
	if rec := adminReq(strictRouter, owned.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("未信任管理员头时 = %d，期望 403", rec.Code)
	}

	// 打开之后，管理员可以代删本租户内他人的资源
	svc := newTestService(t, func(cfg *config.Config) { cfg.Access.TrustAdminHeader = true })
	target := uploadForTenant(t, svc, "crm", "acct-a", "alice")
	router := newTestRouter(t, svc)
	if rec := adminReq(router, target.ID); rec.Code != http.StatusOK {
		t.Fatalf("管理员代删 = %d，期望 200，body = %s", rec.Code, rec.Body.String())
	}

	// 但租户边界仍然有效：别的租户的管理员看到的是 404
	other := uploadForTenant(t, svc, "crm", "acct-b", "bob")
	req := httptest.NewRequest(http.MethodDelete, transport.RouteGroup+"/"+other.ID, nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set(transport.HeaderUserID, "ops")
	req.Header.Set(transport.HeaderIsAdmin, "1")
	if rec := serve(router, req); rec.Code != http.StatusNotFound {
		t.Errorf("跨租户管理员删除 = %d，期望 404", rec.Code)
	}

	// 普通用户带上这个头也没用——它只在被信任时才读，且值必须是真
	normal := httptest.NewRequest(http.MethodDelete, transport.RouteGroup+"/"+other.ID, nil)
	normal.Header.Set(transport.HeaderNamespace, "crm")
	normal.Header.Set(transport.HeaderAccountID, "acct-b")
	normal.Header.Set(transport.HeaderUserID, "mallory")
	normal.Header.Set(transport.HeaderIsAdmin, "false")
	if rec := serve(router, normal); rec.Code != http.StatusForbidden {
		t.Errorf("非管理员删除他人资源 = %d，期望 403", rec.Code)
	}
}

func TestSharedResourceIsReadableAcrossTenants(t *testing.T) {
	svc := newTestService(t, nil)
	ctx := context.Background()
	result, err := svc.Manager.Upload(ctx, &model.UploadRequest{
		FileName:  "default-avatar.png",
		Reader:    bytes.NewReader(pngBytes()),
		BizType:   "image",
		Namespace: "platform",
		AccountID: "root",
		Creator:   "ops",
		Shared:    true,
		IsAdmin:   true,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !result.Resource.Shared {
		t.Fatal("admin upload with shared=true should be marked shared")
	}

	other := &model.AccessRequest{Namespace: "crm", AccountID: "acct-x", UserID: "u-9"}
	if _, err = svc.Manager.Get(ctx, result.Resource.ID, other); err != nil {
		t.Fatalf("another tenant should be able to read a shared resource: %v", err)
	}

	// 读放行、写不放行：挪到 authorizeMutate 就是跨租户写入漏洞。
	if err = svc.Manager.Delete(ctx, result.Resource.ID, other); err == nil {
		t.Fatal("another tenant must not be able to delete a shared resource")
	}
	if _, err = svc.Manager.Update(ctx, result.Resource.ID,
		&model.UpdateRequest{Name: strPtr("hijacked.png")}, other); err == nil {
		t.Fatal("another tenant must not be able to update a shared resource")
	}
}

// 普通调用方设 shared 会被静默丢弃，否则任何业务都能把资源变成全局可读。
func TestSharedRequiresAdmin(t *testing.T) {
	svc := newTestService(t, nil)
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName:  "sneaky.png",
		Reader:    bytes.NewReader(pngBytes()),
		BizType:   "image",
		Namespace: "crm",
		AccountID: "acct-a",
		Shared:    true,
		IsAdmin:   false,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if result.Resource.Shared {
		t.Fatal("a non-admin caller must not be able to publish a shared resource")
	}
	other := &model.AccessRequest{Namespace: "crm", AccountID: "acct-b", UserID: "u-2"}
	if _, err = svc.Manager.Get(context.Background(), result.Resource.ID, other); err == nil {
		t.Fatal("the resource should still be tenant-scoped")
	}
}

func TestAdminScopeIsLimitedToBizTypes(t *testing.T) {
	svc := newTestService(t, nil)
	ctx := context.Background()

	mk := func(bizType string) string {
		result, err := svc.Manager.Upload(ctx, &model.UploadRequest{
			FileName:  "f.png",
			Reader:    bytes.NewReader(pngBytes()),
			BizType:   bizType,
			Namespace: "crm",
			AccountID: "acct-a",
			Creator:   "someone-else",
		})
		if err != nil {
			t.Fatalf("upload %s: %v", bizType, err)
		}
		return result.Resource.ID
	}
	material := mk("material")
	contract := mk("contract")

	admin := &model.AccessRequest{
		Namespace:     "crm",
		AccountID:     "acct-a",
		UserID:        "ops",
		IsAdmin:       true,
		AdminBizTypes: []string{"material"},
	}
	if err := svc.Manager.Delete(ctx, material, admin); err != nil {
		t.Fatalf("admin should be able to delete material: %v", err)
	}
	if err := svc.Manager.Delete(ctx, contract, admin); err == nil {
		t.Fatal("a material admin must not be able to delete contract attachments")
	}
}

// 不限定 biz_type 时保持原有的全租户语义，老接入方不受影响。
func TestUnscopedAdminKeepsFullAuthority(t *testing.T) {
	svc := newTestService(t, nil)
	ctx := context.Background()
	result, err := svc.Manager.Upload(ctx, &model.UploadRequest{
		FileName:  "f.png",
		Reader:    bytes.NewReader(pngBytes()),
		BizType:   "contract",
		Namespace: "crm",
		AccountID: "acct-a",
		Creator:   "someone-else",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	admin := &model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "ops", IsAdmin: true}
	if err = svc.Manager.Delete(ctx, result.Resource.ID, admin); err != nil {
		t.Fatalf("unscoped admin should keep full authority: %v", err)
	}
}
