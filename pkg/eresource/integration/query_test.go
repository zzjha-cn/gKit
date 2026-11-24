package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// 修复点 12：Store 返回的必须是副本，调用方修改不得污染共享状态。
func TestStoreReturnsDefensiveCopies(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "a.txt", []byte("v"))

	fetched, err := svc.Manager.Get(context.Background(), result.Resource.ID, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	fetched.Name = "tampered"
	fetched.Status = model.ResourceStatusDeleted

	again, err := svc.Manager.Get(context.Background(), result.Resource.ID, nil)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if again.Name == "tampered" || again.Status != model.ResourceStatusActive {
		t.Errorf("store state was mutated through returned pointer: %+v", again)
	}
}

// 修复点 17：List 之外还要能拿到总数。
func TestCount(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) { cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPublic })
	for i := 0; i < 5; i++ {
		upload(t, svc, fmt.Sprintf("f%d.txt", i), []byte(fmt.Sprintf("body-%d", i)))
	}
	query := &model.Query{BizType: "image", Limit: 2}
	items, err := svc.Manager.List(context.Background(), query)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("len(items) = %d, want 2", len(items))
	}
	total, err := svc.Manager.Count(context.Background(), query)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if total, err = svc.Manager.Count(context.Background(), &model.Query{BizType: "nope"}); err != nil || total != 0 {
		t.Errorf("count of empty biz = %d, err = %v", total, err)
	}
}

// 列表接口没法「先查出来再判断」，隔离只能靠强制收窄检索条件，
// 而这个条件绝不能让客户端自己指定。
func TestListIsScopedToCaller(t *testing.T) {
	svc := newTestService(t, nil)
	uploadForTenant(t, svc, "crm", "acct-a", "alice")
	uploadForTenant(t, svc, "crm", "acct-b", "bob")

	router := newTestRouter(t, svc)
	// 调用方身份是 acct-a，却试图用 query 参数把范围改成 acct-b
	req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/list?account_id=acct-b&namespace=crm", nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	rec := serve(router, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Data struct {
			Total int64             `json:"total"`
			Items []*model.Resource `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listed.Data.Total != 1 || len(listed.Data.Items) != 1 {
		t.Fatalf("total = %d, items = %d; query params must not widen the scope", listed.Data.Total, len(listed.Data.Items))
	}
	if got := listed.Data.Items[0].AccountID; got != "acct-a" {
		t.Errorf("listed account_id = %q, want acct-a", got)
	}
}

// 元数据接口不得泄露底层存储布局。
func TestGetResponseHidesStorageLayout(t *testing.T) {
	svc := newTestService(t, nil)
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")
	if owned.StorePath == "" {
		t.Fatal("fixture should have a store path")
	}
	router := newTestRouter(t, svc)
	req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+owned.ID, nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set("X-User-Id", "alice")
	rec := serve(router, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, owned.StorePath) {
		t.Errorf("response leaks store path %q: %s", owned.StorePath, body)
	}
	if strings.Contains(body, "store_path") {
		t.Errorf("response still carries a store_path field: %s", body)
	}
	// 业务真正需要的字段仍然在
	if !strings.Contains(body, owned.ID) {
		t.Errorf("response lost the resource id: %s", body)
	}
}

// 存储布局不外泄：object_key 带着完整的租户 / 业务 / 年月分桶结构，
// 客户端用 id 就够了。
func TestResourceViewHidesStorageLayout(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadOwned(t, svc, nil)
	router := newTestRouter(t, svc)

	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(transport.HeaderNamespace, "crm")
		req.Header.Set(transport.HeaderAccountID, "acct-a")
		req.Header.Set(transport.HeaderUserID, "alice")
		rec := serve(router, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// object key 的首段是租户名，整条 key 不该出现在任何读接口的响应里。
	for _, path := range []string{
		transport.RouteGroup + "/" + resource.ID,
		transport.RouteGroup + "/list",
	} {
		body := get(path)
		for _, leaked := range []string{resource.ObjectKey, resource.StorePath} {
			if leaked != "" && strings.Contains(body, leaked) {
				t.Errorf("%s leaks %q", path, leaked)
			}
		}
	}

	// 服务端自己仍然拿得到：抹掉只发生在交给客户端的那一份副本上。
	stored, err := svc.Manager.Get(context.Background(), resource.ID, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.ObjectKey == "" {
		t.Error("server-side record must keep the object key")
	}
}

// uploadTagged 上传一条带指定标签与 metadata 的资源。
func uploadTagged(t *testing.T, svc *eresource.Service, name string, tags []string, metadata map[string]string) *model.Resource {
	t.Helper()
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: name, Reader: strings.NewReader("body-" + name),
		Namespace: "crm", AccountID: "acct-a", BizType: "doc",
		AccessPolicy: model.AccessPolicyProtected, Creator: "alice",
		Tags: tags, Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	return result.Resource
}

// uploadSharedContent 上传一份固定内容：多次调用必然命中去重。
func uploadSharedContent(t *testing.T, svc *eresource.Service, name string) *model.Resource {
	t.Helper()
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: name, Reader: strings.NewReader(sharedBody),
		Namespace: "crm", AccountID: "acct-a", BizType: "doc",
		AccessPolicy: model.AccessPolicyProtected, Creator: "alice",
	})
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	return result.Resource
}

// sharedBody 去重类测试共用的文件内容。
const sharedBody = "shared-dedup-body"

func listIDs(t *testing.T, svc *eresource.Service, query *model.Query) []string {
	t.Helper()
	found, err := svc.Manager.List(context.Background(), query)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := make([]string, 0, len(found))
	for _, resource := range found {
		ids = append(ids, resource.ID)
	}
	return ids
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// T6：标签写得进就必须查得出，且是 AND 语义——多加一个条件只会让结果更少。
func TestListFiltersByTags(t *testing.T) {
	svc := newTestService(t, nil)
	both := uploadTagged(t, svc, "a.txt", []string{"campaign-001", "q4"}, nil)
	one := uploadTagged(t, svc, "b.txt", []string{"campaign-001"}, nil)

	single := listIDs(t, svc, &model.Query{Namespace: "crm", AccountID: "acct-a", Tags: []string{"campaign-001"}})
	if !containsID(single, both.ID) || !containsID(single, one.ID) {
		t.Errorf("single tag should match both, got %v", single)
	}
	narrowed := listIDs(t, svc, &model.Query{Namespace: "crm", AccountID: "acct-a", Tags: []string{"campaign-001", "q4"}})
	if !containsID(narrowed, both.ID) || containsID(narrowed, one.ID) {
		t.Errorf("tags must be AND, got %v", narrowed)
	}
}

// B7：不写 Go 的接入方要靠 metadata 按自己的业务维度查资源。
func TestListFiltersByMetadata(t *testing.T) {
	svc := newTestService(t, nil)
	target := uploadTagged(t, svc, "a.txt", nil, map[string]string{"order_id": "A1", "channel": "wechat"})
	other := uploadTagged(t, svc, "b.txt", nil, map[string]string{"order_id": "B2", "channel": "wechat"})

	matched := listIDs(t, svc, &model.Query{
		Namespace: "crm", AccountID: "acct-a",
		Metadata: map[string]string{"order_id": "A1"},
	})
	if !containsID(matched, target.ID) || containsID(matched, other.ID) {
		t.Errorf("metadata filter failed, got %v", matched)
	}
	// 多个 key 之间是 AND：对不上的组合查不出东西。
	none := listIDs(t, svc, &model.Query{
		Namespace: "crm", AccountID: "acct-a",
		Metadata: map[string]string{"order_id": "A1", "channel": "sms"},
	})
	if len(none) != 0 {
		t.Errorf("metadata conditions must be AND, got %v", none)
	}
}

// metadata 的 key 会被拼进存储层的字段路径，"$" 与 "." 必须在入口挡住：
// 前者能把一个等值条件变成任意查询操作符，后者能越过 metadata 这一层。
func TestQueryRejectsDangerousMetadataKeys(t *testing.T) {
	svc := newTestService(t, nil)
	cases := []string{"$where", "$gt", "nested.key", ""}
	for _, key := range cases {
		t.Run(fmt.Sprintf("key=%q", key), func(t *testing.T) {
			_, err := svc.Manager.List(context.Background(), &model.Query{
				Metadata: map[string]string{key: "x"},
			})
			if !errors.Is(err, model.ErrInvalidQuery) {
				t.Fatalf("err = %v, want ErrInvalidQuery", err)
			}
		})
	}
	// 写入侧用的是同一把尺子。
	_, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "a.txt", Reader: strings.NewReader("x"),
		Metadata: map[string]string{"$set": "1"},
	})
	if !errors.Is(err, model.ErrInvalidQuery) {
		t.Fatalf("upload err = %v, want ErrInvalidQuery", err)
	}
}

// 检索条件的规模必须有上限，否则单个请求就能构造出代价不可控的查询。
func TestQueryRejectsOversizedConditions(t *testing.T) {
	svc := newTestService(t, nil)
	tooMany := make([]string, model.MaxQueryTags+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("tag-%d", i)
	}
	if _, err := svc.Manager.List(context.Background(), &model.Query{Tags: tooMany}); !errors.Is(err, model.ErrInvalidQuery) {
		t.Errorf("tags err = %v, want ErrInvalidQuery", err)
	}
	ids := make([]string, model.MaxQueryIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}
	if _, err := svc.Manager.List(context.Background(), &model.Query{IDs: ids}); !errors.Is(err, model.ErrInvalidQuery) {
		t.Errorf("ids err = %v, want ErrInvalidQuery", err)
	}
}

// T9：ids / created_after / created_before 三个条件 Store 早就支持，HTTP 层要暴露出来。
func TestListOverHTTPExposesEveryDimension(t *testing.T) {
	svc := newTestService(t, nil)
	target := uploadTagged(t, svc, "a.txt", []string{"campaign-001"}, map[string]string{"order_id": "A1"})
	other := uploadTagged(t, svc, "b.txt", []string{"campaign-002"}, map[string]string{"order_id": "B2"})
	router := newTestRouter(t, svc)

	list := func(query string) []string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/list?"+query, nil)
		req.Header.Set(transport.HeaderNamespace, "crm")
		req.Header.Set(transport.HeaderAccountID, "acct-a")
		req.Header.Set(transport.HeaderUserID, "alice")
		rec := serve(router, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("query %q: status = %d, body = %s", query, rec.Code, rec.Body.String())
		}
		var payload struct {
			Data struct {
				Items []*model.Resource `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids := make([]string, 0, len(payload.Data.Items))
		for _, item := range payload.Data.Items {
			ids = append(ids, item.ID)
		}
		return ids
	}

	cases := map[string]struct {
		query string
		want  string
		deny  string
	}{
		"按 id 批量":    {"ids=" + target.ID, target.ID, other.ID},
		"按标签":        {"tags=campaign-001", target.ID, other.ID},
		"按 metadata": {"metadata.order_id=A1", target.ID, other.ID},
		"按时间段": {fmt.Sprintf("created_after=%s",
			time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)), target.ID, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ids := list(tc.query)
			if !containsID(ids, tc.want) {
				t.Errorf("want %s in %v", tc.want, ids)
			}
			if tc.deny != "" && containsID(ids, tc.deny) {
				t.Errorf("did not want %s in %v", tc.deny, ids)
			}
		})
	}

	// 未来的时间段应当什么都查不到，确认条件真的生效了而不是被忽略。
	future := list("created_after=" + time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if len(future) != 0 {
		t.Errorf("future range should be empty, got %v", future)
	}
	// 非法 metadata key 在 HTTP 上是 400 而不是 500。
	req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/list?metadata.$where=1", nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	if rec := serve(router, req); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// 上传接口要能把 metadata 传进来，否则 B7 只补了一半。
func TestUploadAcceptsMetadataOverHTTP(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)

	body, contentType := multipartUpload(t, "a.txt", []byte("hello"), map[string]string{
		"biz_type":           "doc",
		"metadata.order_id":  "A1",
		"metadata.signed_by": "alice",
	})
	req := httptest.NewRequest(http.MethodPost, transport.RouteGroup+"/upload", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set(transport.HeaderUserID, "alice")
	rec := serve(router, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data model.UploadResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	metadata := payload.Data.Resource.Metadata
	if metadata["order_id"] != "A1" || metadata["signed_by"] != "alice" {
		t.Fatalf("metadata = %v", metadata)
	}

	// 非法 key 是调用方把参数写错了，必须是 400 而不是 500。
	body, contentType = multipartUpload(t, "b.txt", []byte("hello"), map[string]string{
		"metadata.$where": "1",
	})
	req = httptest.NewRequest(http.MethodPost, transport.RouteGroup+"/upload", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set(transport.HeaderUserID, "alice")
	if rec = serve(router, req); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// multipartUpload 构造一个带附加表单字段的上传请求体。
func multipartUpload(t *testing.T, name string, content []byte, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(transport.FormFieldFile, name)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	for key, value := range fields {
		if err = writer.WriteField(key, value); err != nil {
			t.Fatalf("write field %s: %v", key, err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return body, writer.FormDataContentType()
}

// B6：命中去重时本次上传的文件名 / 标签 / metadata 不能被丢掉。
func TestDedupNewRecordKeepsThisUploadsFields(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.EnableDedup = true
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
	})
	first := uploadTagged(t, svc, "a.png", []string{"campaign-001"}, map[string]string{"order_id": "A1"})

	// 同内容、同租户、同维度，但带着完全不同的业务字段。
	second, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "b.png", Reader: strings.NewReader("body-a.png"),
		Namespace: "crm", AccountID: "acct-a", BizType: "doc",
		AccessPolicy: model.AccessPolicyProtected, Creator: "alice",
		Tags: []string{"campaign-002"}, Metadata: map[string]string{"order_id": "B2"},
	})
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if !second.Existed || second.Uploaded {
		t.Fatalf("expected a dedup hit, existed=%t uploaded=%t", second.Existed, second.Uploaded)
	}
	if second.Resource.ID == first.ID {
		t.Fatal("new_record must not return the historical record")
	}
	if second.Resource.Name != "b.png" {
		t.Errorf("name = %q, want b.png", second.Resource.Name)
	}
	if strings.Join(second.Resource.Tags, ",") != "campaign-002" {
		t.Errorf("tags = %v", second.Resource.Tags)
	}
	if second.Resource.Metadata["order_id"] != "B2" {
		t.Errorf("metadata = %v", second.Resource.Metadata)
	}
	// 省下来的是底层写入：两条记录指向同一个对象。
	if second.Resource.ObjectKey != first.ObjectKey {
		t.Errorf("object key = %q, want %q", second.Resource.ObjectKey, first.ObjectKey)
	}
	// 历史记录不能被这次上传改动。
	stored, err := svc.Manager.Get(context.Background(), first.ID, nil)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if stored.Name != "a.png" || stored.Metadata["order_id"] != "A1" {
		t.Errorf("historical record was mutated: %+v", stored)
	}
}

// reuse_record 仍然可选，行为与升级前一致。
func TestDedupReuseRecordStrategy(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.EnableDedup = true
		cfg.Manager.DedupStrategy = config.DedupStrategyReuseRecord
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
	})
	first := uploadSharedContent(t, svc, "a.png")
	second := uploadSharedContent(t, svc, "b.png")
	if second.ID != first.ID {
		t.Fatalf("reuse_record should return the historical record, got %q want %q", second.ID, first.ID)
	}
}

// new_record 把「记录 : 对象」变成了 N:1，删除必须先数引用——
// 否则删掉一条记录就让所有兄弟记录一起 404。
func TestDeleteKeepsObjectWhileOtherRecordsReferenceIt(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.EnableDedup = true
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
	})
	first := uploadSharedContent(t, svc, "a.png")
	second := uploadSharedContent(t, svc, "b.png")
	if first.ObjectKey != second.ObjectKey {
		t.Fatalf("expected a shared object")
	}
	owner := &model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "alice"}

	if err := svc.Manager.Delete(context.Background(), first.ID, owner); err != nil {
		t.Fatalf("delete first: %v", err)
	}
	// 兄弟记录还在，对象必须还在。
	assertFileContent(t, second.StorePath, sharedBody)

	if err := svc.Manager.Delete(context.Background(), second.ID, owner); err != nil {
		t.Fatalf("delete second: %v", err)
	}
	stat, err := svc.Provider.Stat(context.Background(), second.ObjectKey)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.Exists {
		t.Error("last reference removed, the object should be gone")
	}
}

// 访问策略改动必须落到底层对象的 ACL 上，否则会得到一条
// 「元数据说公开、对象还是私有」的资源（反向则是一次静默泄露）。
func TestUpdateAccessPolicySyncsObjectACL(t *testing.T) {
	p := newMemProvider()
	dir := t.TempDir()
	svc, err := eresource.New(config.Config{
		Manager: config.ManagerConfig{DefaultAccessPolicy: model.AccessPolicyPrivate, MaxFileSize: 1024},
		Store:   config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(dir, "meta.json")},
		Provider: config.ProviderConfig{
			Type: provider.TypeLocal, RootDir: filepath.Join(dir, "objects"),
			Domain: "cdn.test", Scheme: "https",
		},
		Access: config.AccessConfig{SignedURLSecret: "unit-test-secret", SignedURLDefaultExpire: 60},
	}, eresource.WithProvider(p))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)

	resource := uploadOwned(t, svc, nil)
	if got := p.aclOf(resource.ObjectKey); got != model.AccessPolicyPrivate {
		t.Fatalf("initial acl = %q, want private", got)
	}
	if _, err = svc.Manager.Update(context.Background(), resource.ID,
		&model.UpdateRequest{AccessPolicy: strptr(model.AccessPolicyPublic)}, ownerOf(resource)); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := p.aclOf(resource.ObjectKey); got != model.AccessPolicyPublic {
		t.Errorf("acl = %q, want public", got)
	}
}
