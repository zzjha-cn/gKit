package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// batchPayload 批量接口的响应结构。
type batchPayload struct {
	Data struct {
		Items []struct {
			ID      string `json:"id"`
			URL     string `json:"url"`
			Deleted bool   `json:"deleted"`
		} `json:"items"`
		Failed []struct {
			ID   string `json:"id"`
			Code int    `json:"code"`
		} `json:"failed"`
	} `json:"data"`
}

func postBatch(t *testing.T, router interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, path, form, userID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set(transport.HeaderUserID, userID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeBatch(t *testing.T, rec *httptest.ResponseRecorder) batchPayload {
	t.Helper()
	var payload batchPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return payload
}

// B8：一个 20 张图的列表页不该发 20 次请求签 20 次名。
func TestBatchAccessSignsEveryID(t *testing.T) {
	svc := newTestService(t, nil)
	first := uploadTagged(t, svc, "a.txt", nil, nil)
	second := uploadTagged(t, svc, "b.txt", nil, nil)
	router := newTestRouter(t, svc)

	rec := postBatch(t, router, transport.RouteGroup+"/batch/access",
		"ids="+first.ID+","+second.ID, "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	payload := decodeBatch(t, rec)
	if len(payload.Data.Items) != 2 || len(payload.Data.Failed) != 0 {
		t.Fatalf("unexpected payload: %+v", payload.Data)
	}
	for _, item := range payload.Data.Items {
		if item.URL == "" {
			t.Errorf("id %s has no url", item.ID)
		}
	}
}

// 单条失败不能把整批拖垮：剩下的 ID 该拿到地址还是要拿到。
func TestBatchAccessReportsPerItemFailures(t *testing.T) {
	svc := newTestService(t, nil)
	ok := uploadTagged(t, svc, "a.txt", nil, nil)
	// 另一个租户的资源：对本次调用方而言应当表现为「不存在」，
	// 而不是 403——否则批量接口就成了一个跨租户的存在性探测器。
	foreign := uploadForTenant(t, svc, "crm", "acct-b", "bob")
	router := newTestRouter(t, svc)

	rec := postBatch(t, router, transport.RouteGroup+"/batch/access",
		"ids="+ok.ID+"&ids=missing-id&ids="+foreign.ID, "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	payload := decodeBatch(t, rec)
	if len(payload.Data.Items) != 1 || payload.Data.Items[0].ID != ok.ID {
		t.Fatalf("items = %+v", payload.Data.Items)
	}
	if len(payload.Data.Failed) != 2 {
		t.Fatalf("failed = %+v", payload.Data.Failed)
	}
	for _, failure := range payload.Data.Failed {
		if failure.Code != http.StatusNotFound {
			t.Errorf("id %s code = %d, want 404", failure.ID, failure.Code)
		}
	}
}

// 批量删除与单条删除同一套归属校验。
func TestBatchDeleteRespectsOwnership(t *testing.T) {
	svc := newTestService(t, nil)
	mine := uploadTagged(t, svc, "a.txt", nil, nil)
	theirs := uploadForTenant(t, svc, "crm", "acct-a", "bob")
	router := newTestRouter(t, svc)

	rec := postBatch(t, router, transport.RouteGroup+"/batch/delete",
		"ids="+mine.ID+","+theirs.ID, "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	payload := decodeBatch(t, rec)
	if len(payload.Data.Items) != 1 || payload.Data.Items[0].ID != mine.ID {
		t.Fatalf("items = %+v", payload.Data.Items)
	}
	if len(payload.Data.Failed) != 1 || payload.Data.Failed[0].Code != http.StatusForbidden {
		t.Fatalf("failed = %+v", payload.Data.Failed)
	}
	// 别人的那条必须完好无损。
	if _, err := svc.Manager.Get(context.Background(), theirs.ID, nil); err != nil {
		t.Errorf("other user's resource was deleted: %v", err)
	}
}

// 批量规模必须有上限，否则一个请求就能把一个 worker 占满。
func TestBatchRejectsEmptyAndOversizedRequests(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)

	if rec := postBatch(t, router, transport.RouteGroup+"/batch/access", "", "alice"); rec.Code != http.StatusBadRequest {
		t.Errorf("empty batch: status = %d, want 400", rec.Code)
	}
	ids := make([]string, manager.MaxBatchSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}
	rec := postBatch(t, router, transport.RouteGroup+"/batch/access", "ids="+strings.Join(ids, ","), "alice")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized batch: status = %d, want 400", rec.Code)
	}
}

// 重复 ID 要折叠掉：同一个 ID 传十次会被签十次名，
// 后九次在批量删除里还会变成一堆「不存在」的失败项。
func TestBatchDeduplicatesIDs(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadTagged(t, svc, "a.txt", nil, nil)
	results, err := svc.Manager.BatchAccessURL(context.Background(),
		[]string{resource.ID, resource.ID, resource.ID},
		&model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "alice"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
}

// 批量接口的入参同样只认可信身份：换个 account_id 翻不到别人的资源。
func TestBatchIsScopedToCaller(t *testing.T) {
	svc := newTestService(t, nil)
	foreign := uploadForTenant(t, svc, "crm", "acct-b", "bob")

	results, err := svc.Manager.BatchAccessURL(context.Background(), []string{foreign.ID},
		&model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "alice"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("cross-tenant id must fail, got %+v", results)
	}
	if !errors.Is(results[0].Err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", results[0].Err)
	}
}
