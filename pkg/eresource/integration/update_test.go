package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

func strptr(s string) *string { return &s }

// uploadOwned 上传一条带完整租户与归属信息的资源，供更新类测试使用。
func uploadOwned(t *testing.T, svc *eresource.Service, tune func(*model.UploadRequest)) *model.Resource {
	t.Helper()
	req := &model.UploadRequest{
		FileName: "draft.txt", Reader: strings.NewReader("draft-body"),
		Namespace: "crm", AccountID: "acct-a", BizType: "doc",
		AccessPolicy: model.AccessPolicyPrivate, Creator: "alice",
		Tags:     []string{"campaign-001"},
		Metadata: map[string]string{"order_id": "A1"},
	}
	if tune != nil {
		tune(req)
	}
	result, err := svc.Manager.Upload(context.Background(), req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return result.Resource
}

func ownerOf(resource *model.Resource) *model.AccessRequest {
	return &model.AccessRequest{
		Namespace: resource.Namespace,
		AccountID: resource.AccountID,
		UserID:    resource.Creator,
	}
}

// B5：改标签、改名、改分类不该再靠「删了重传」，ID 与 URL 必须保持不变。
func TestUpdateChangesBusinessFieldsInPlace(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, nil)

	updated, err := svc.Manager.Update(context.Background(), resource.ID, &model.UpdateRequest{
		Name:    strptr("final.txt"),
		BizType: strptr("contract"),
		Tags:    []string{"campaign-002", "q4"},
	}, ownerOf(resource))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.ID != resource.ID {
		t.Errorf("id changed: %q -> %q", resource.ID, updated.ID)
	}
	if updated.Name != "final.txt" || updated.BizType != "contract" {
		t.Errorf("name/biz_type not updated: %+v", updated)
	}
	// 返回的副本必须带上真正落库的更新时间，而不是零值。
	if !updated.UpdatedAt.After(resource.UpdatedAt) {
		t.Errorf("updated_at = %v, want later than %v", updated.UpdatedAt, resource.UpdatedAt)
	}
	if strings.Join(updated.Tags, ",") != "campaign-002,q4" {
		t.Errorf("tags = %v", updated.Tags)
	}
	// 存储布局与内容事实必须纹丝不动：改 biz_type 不会把对象搬家。
	if updated.ObjectKey != resource.ObjectKey || updated.Hash != resource.Hash || updated.Size != resource.Size {
		t.Errorf("storage facts changed: %+v", updated)
	}
	// 落库了才算改完。
	stored, err := svc.Manager.Get(context.Background(), resource.ID, ownerOf(resource))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Name != "final.txt" {
		t.Errorf("stored name = %q", stored.Name)
	}
}

// metadata 是合并语义：只动传进来的 key，空值删除该 key。
// 整体替换会让调用方拿一份过期快照回写时，把并发写入的其它 key 一起抹掉。
func TestUpdateMergesMetadata(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, func(r *model.UploadRequest) {
		r.Metadata = map[string]string{"order_id": "A1", "channel": "wechat"}
	})

	updated, err := svc.Manager.Update(context.Background(), resource.ID, &model.UpdateRequest{
		Metadata: map[string]string{"order_id": "B2", "owner": "ops", "channel": ""},
	}, ownerOf(resource))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Metadata["order_id"] != "B2" {
		t.Errorf("order_id = %q, want B2", updated.Metadata["order_id"])
	}
	if updated.Metadata["owner"] != "ops" {
		t.Errorf("owner = %q, want ops", updated.Metadata["owner"])
	}
	if _, exists := updated.Metadata["channel"]; exists {
		t.Error("empty value should delete the key")
	}
}

// tags 是整体替换：传一个空集合就是清空。
func TestUpdateReplacesTags(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, nil)

	updated, err := svc.Manager.Update(context.Background(), resource.ID,
		&model.UpdateRequest{Tags: []string{}}, ownerOf(resource))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(updated.Tags) != 0 {
		t.Errorf("tags = %v, want empty", updated.Tags)
	}
}

// 不可变字段压根不在请求结构里，这里守住的是「更新不会顺手改掉归属」。
func TestUpdateKeepsOwnershipAndLayout(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, nil)

	updated, err := svc.Manager.Update(context.Background(), resource.ID,
		&model.UpdateRequest{Name: strptr("renamed.txt")}, ownerOf(resource))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, pair := range []struct {
		field     string
		got, want string
	}{
		{"namespace", updated.Namespace, resource.Namespace},
		{"account_id", updated.AccountID, resource.AccountID},
		{"creator", updated.Creator, resource.Creator},
		{"provider", updated.Provider, resource.Provider},
		{"object_key", updated.ObjectKey, resource.ObjectKey},
		{"store_path", updated.StorePath, resource.StorePath},
		{"hash", updated.Hash, resource.Hash},
		{"mime_type", updated.MimeType, resource.MimeType},
		{"ext", updated.Ext, resource.Ext},
	} {
		if pair.got != pair.want {
			t.Errorf("%s = %q, want %q", pair.field, pair.got, pair.want)
		}
	}
}

// 草稿转发布：private -> public 要同时写出固定直链；反向必须把直链清掉，
// 否则之后每一次 Get / List 都会继续把底层地址交出去，策略等于没改。
func TestUpdateAccessPolicyRewritesPublicURL(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, nil)
	if resource.URL != "" {
		t.Fatalf("private resource should not carry a public url, got %q", resource.URL)
	}

	published, err := svc.Manager.Update(context.Background(), resource.ID,
		&model.UpdateRequest{AccessPolicy: strptr(model.AccessPolicyPublic)}, ownerOf(resource))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if published.AccessPolicy != model.AccessPolicyPublic || published.URL == "" {
		t.Fatalf("publish did not take effect: policy=%q url=%q", published.AccessPolicy, published.URL)
	}

	retracted, err := svc.Manager.Update(context.Background(), resource.ID,
		&model.UpdateRequest{AccessPolicy: strptr(model.AccessPolicyPrivate)}, ownerOf(resource))
	if err != nil {
		t.Fatalf("retract: %v", err)
	}
	if retracted.URL != "" {
		t.Errorf("public url must be cleared when leaving the public policy, got %q", retracted.URL)
	}
}

// 「先传成 private，再改成 public」不能成为危险内容拦截的绕过路径。
func TestUpdateKeepsPublicContentGuard(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, func(r *model.UploadRequest) {
		r.FileName = "page.html"
		r.ContentType = "text/html"
		r.Reader = strings.NewReader("<html><script>alert(1)</script></html>")
		r.AccessPolicy = model.AccessPolicyPrivate
	})

	_, err := svc.Manager.Update(context.Background(), resource.ID,
		&model.UpdateRequest{AccessPolicy: strptr(model.AccessPolicyPublic)}, ownerOf(resource))
	if !errors.Is(err, manager.ErrUnsafePublicContent) {
		t.Fatalf("err = %v, want ErrUnsafePublicContent", err)
	}
}

// 更新与删除同一套权限口径：别人的资源改不了。
func TestUpdateRequiresOwnership(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, nil)

	cases := map[string]struct {
		req  *model.AccessRequest
		want error
	}{
		"另一个用户":  {&model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "bob"}, manager.ErrNotResourceOwner},
		"另一个租户":  {&model.AccessRequest{Namespace: "crm", AccountID: "acct-b", UserID: "alice"}, manager.ErrResourceNotVisible},
		"本租户管理员": {&model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "bob", IsAdmin: true}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Manager.Update(context.Background(), resource.ID,
				&model.UpdateRequest{Name: strptr("x.txt")}, tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// 软删除之后的资源不可更新：否则一次 PATCH 就能把它从回收站里捞回来，
// 绕过删除权限本身。
func TestUpdateRejectsDeletedResource(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) { cfg.Access.SoftDelete = true })
	resource := uploadOwned(t, svc, nil)
	if err := svc.Manager.Delete(context.Background(), resource.ID, ownerOf(resource)); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := svc.Manager.Update(context.Background(), resource.ID,
		&model.UpdateRequest{Name: strptr("back.txt")}, ownerOf(resource))
	if !errors.Is(err, manager.ErrResourceInactive) {
		t.Fatalf("err = %v, want ErrResourceInactive", err)
	}
}

// 文件名会进下载响应的 Content-Disposition，控制字符必须挡在入口。
func TestUpdateRejectsInvalidInput(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, nil)

	cases := map[string]struct {
		update *model.UpdateRequest
		want   error
	}{
		"空请求":       {&model.UpdateRequest{}, manager.ErrEmptyUpdate},
		"空文件名":      {&model.UpdateRequest{Name: strptr("  ")}, manager.ErrInvalidResourceName},
		"文件名带换行":    {&model.UpdateRequest{Name: strptr("a\r\nX-Evil: 1.txt")}, manager.ErrInvalidResourceName},
		"文件名带路径":    {&model.UpdateRequest{Name: strptr("../../etc/passwd")}, manager.ErrInvalidResourceName},
		"非法访问策略":    {&model.UpdateRequest{AccessPolicy: strptr("open")}, manager.ErrInvalidAccessPolicy},
		"非法 biz":    {&model.UpdateRequest{BizType: strptr("../evil")}, provider.ErrInvalidSegment},
		"metadata键": {&model.UpdateRequest{Metadata: map[string]string{"$where": "1"}}, model.ErrInvalidQuery},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Manager.Update(context.Background(), resource.ID, tc.update, ownerOf(resource))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// 多条记录共享同一个对象时（new_record 去重的必然结果），
// 改其中一条的访问策略会连带改掉兄弟记录的实际可见性，必须拒绝。
func TestUpdateRejectsPolicyChangeOnSharedObject(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.EnableDedup = true
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
	})
	first := uploadOwned(t, svc, nil)
	second := uploadOwned(t, svc, nil) // 同内容同维度，命中去重，复用同一个对象
	if first.ObjectKey != second.ObjectKey {
		t.Fatalf("expected a shared object, got %q and %q", first.ObjectKey, second.ObjectKey)
	}

	_, err := svc.Manager.Update(context.Background(), first.ID,
		&model.UpdateRequest{AccessPolicy: strptr(model.AccessPolicyPublic)}, ownerOf(first))
	if !errors.Is(err, manager.ErrObjectShared) {
		t.Fatalf("err = %v, want ErrObjectShared", err)
	}
	// 共享对象上改不了策略，但纯业务字段照常可改。
	if _, err = svc.Manager.Update(context.Background(), first.ID,
		&model.UpdateRequest{Tags: []string{"ok"}}, ownerOf(first)); err != nil {
		t.Fatalf("tag update on shared object: %v", err)
	}
}

// PATCH 走一遍完整的 HTTP 链路，顺带确认身份只认请求头。
func TestUpdateOverHTTP(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, nil)
	router := newTestRouter(t, svc)

	patch := func(id string, form string, userID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, transport.RouteGroup+"/"+id, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set(transport.HeaderNamespace, "crm")
		req.Header.Set(transport.HeaderAccountID, "acct-a")
		req.Header.Set(transport.HeaderUserID, userID)
		return serve(router, req)
	}

	rec := patch(resource.ID, "name=final.txt&tags=a&tags=b&metadata.order_id=B2", "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	data := decodeResource(t, rec)
	if data.Name != "final.txt" || strings.Join(data.Tags, ",") != "a,b" || data.Metadata["order_id"] != "B2" {
		t.Errorf("unexpected payload: %+v", data)
	}
	// 响应与 Get / List 一样必须脱敏。
	if data.StorePath != "" || data.Bucket != "" {
		t.Errorf("response leaks storage layout: store_path=%q bucket=%q", data.StorePath, data.Bucket)
	}

	if rec = patch(resource.ID, "name=hijacked.txt", "bob"); rec.Code != http.StatusForbidden {
		t.Errorf("another user: status = %d, want 403", rec.Code)
	}
	if rec = patch(resource.ID, "access_policy=open", "alice"); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid policy: status = %d, want 400", rec.Code)
	}
	if rec = patch(resource.ID, "", "alice"); rec.Code != http.StatusBadRequest {
		t.Errorf("empty update: status = %d, want 400", rec.Code)
	}
}

// decodeResource 解析单条资源响应。
func decodeResource(t *testing.T, rec *httptest.ResponseRecorder) *model.Resource {
	t.Helper()
	var payload struct {
		Data *model.Resource `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if payload.Data == nil {
		t.Fatalf("missing data in %s", rec.Body.String())
	}
	return payload.Data
}
