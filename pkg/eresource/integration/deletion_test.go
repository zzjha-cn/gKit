package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// 修复点 7：底层文件被外部删掉后，元数据仍然要能删除。
func TestDeleteSucceedsWhenObjectAlreadyGone(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "gone.txt", []byte("bye"))
	if err := os.Remove(result.Resource.StorePath); err != nil {
		t.Fatalf("remove object: %v", err)
	}
	if err := svc.Manager.Delete(context.Background(), result.Resource.ID, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Manager.Get(context.Background(), result.Resource.ID, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// 删除会连底层对象一起抹掉且不可撤销，默认只有上传者本人能做。
func TestDeleteRequiresCreator(t *testing.T) {
	svc := newTestService(t, nil)
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")
	sameTenant := func(user string) *model.AccessRequest {
		return &model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: user}
	}

	// 同租户但不是上传者：拒绝，且必须与「跨租户」区分开（403 vs 404）
	if err := svc.Manager.Delete(context.Background(), owned.ID, sameTenant("bob")); !errors.Is(err, manager.ErrNotResourceOwner) {
		t.Fatalf("delete by non-creator = %v, want ErrNotResourceOwner", err)
	}
	// 没删掉
	if _, err := svc.Manager.Get(context.Background(), owned.ID, sameTenant("alice")); err != nil {
		t.Fatalf("resource should still exist: %v", err)
	}
	// 上传者本人：放行
	if err := svc.Manager.Delete(context.Background(), owned.ID, sameTenant("alice")); err != nil {
		t.Fatalf("delete by creator: %v", err)
	}

	// 后台场景显式放开之后，代删放行，但仍受租户边界约束
	admin := newTestService(t, func(cfg *config.Config) { cfg.Access.AllowCrossCreatorDelete = true })
	target := uploadForTenant(t, admin, "crm", "acct-a", "alice")
	if err := admin.Manager.Delete(context.Background(), target.ID, sameTenant("ops")); err != nil {
		t.Fatalf("delete with allow_cross_creator_delete: %v", err)
	}
	other := uploadForTenant(t, admin, "crm", "acct-a", "alice")
	outsider := &model.AccessRequest{Namespace: "crm", AccountID: "acct-b", UserID: "ops"}
	if err := admin.Manager.Delete(context.Background(), other.ID, outsider); !errors.Is(err, manager.ErrResourceNotVisible) {
		t.Fatalf("cross-tenant delete = %v, want ErrResourceNotVisible even with allow_cross_creator_delete", err)
	}
}

// B4：硬删除不可撤销，误删即永久数据丢失。软删除保留回收窗口，
// 窗口之外由 PurgeDeleted 真删。
func TestSoftDeleteKeepsObjectUntilPurge(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Access.SoftDelete = true
	})
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")
	caller := &model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "alice"}

	if err := svc.Manager.Delete(context.Background(), owned.ID, caller); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// 对调用方而言与删除无异
	if _, err := svc.Manager.Get(context.Background(), owned.ID, caller); !errors.Is(err, manager.ErrResourceInactive) {
		t.Fatalf("软删后 Get err = %v, want ErrResourceInactive", err)
	}
	// 但底层对象还在，元数据可恢复
	if _, err := os.Stat(owned.StorePath); err != nil {
		t.Fatalf("软删不应抹掉底层对象：%v", err)
	}
	stored, err := svc.Store.GetByID(context.Background(), owned.ID)
	if err != nil {
		t.Fatalf("软删后元数据应当保留：%v", err)
	}
	if stored.Status != model.ResourceStatusDeleted {
		t.Errorf("status = %q, want deleted", stored.Status)
	}

	// 回收窗口内不清理
	mgr, ok := svc.Manager.(*manager.DefaultManager)
	if !ok {
		t.Fatalf("manager type = %T", svc.Manager)
	}
	purged, err := mgr.PurgeDeleted(context.Background(), time.Now().Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 0 {
		t.Fatalf("回收窗口内清理了 %d 条，期望 0", purged)
	}

	// 窗口之外真删
	if purged, err = mgr.PurgeDeleted(context.Background(), time.Now().Add(time.Hour), 0); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if _, err = os.Stat(owned.StorePath); !os.IsNotExist(err) {
		t.Errorf("purge 之后底层对象仍在：%v", err)
	}
	if _, err = svc.Store.GetByID(context.Background(), owned.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("purge 之后元数据仍在：%v", err)
	}
}

// 软删除的资源默认不出现在列表里：删掉的东西还在列表上，对业务来说就是没删掉。
func TestListHidesSoftDeletedByDefault(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Access.SoftDelete = true
	})
	owned := uploadForTenant(t, svc, "crm", "acct-a", "alice")
	if err := svc.Manager.Delete(context.Background(), owned.ID,
		&model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "alice"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	router := newTestRouter(t, svc)

	listed := func(query string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/list"+query, nil)
		req.Header.Set(transport.HeaderNamespace, "crm")
		req.Header.Set(transport.HeaderAccountID, "acct-a")
		rec := serve(router, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Data struct {
				Total int `json:"total"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return resp.Data.Total
	}

	if got := listed(""); got != 0 {
		t.Errorf("默认列表 total = %d，期望 0（已软删）", got)
	}
	if got := listed("?status=all"); got != 1 {
		t.Errorf("status=all 时 total = %d，期望 1", got)
	}
}

// 打开软删除却没配保留期时补默认值：不允许出现「软删了却立刻可被清理」。
func TestSoftDeleteGetsDefaultRetention(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Access.SoftDelete = true
	cfg.Normalize()
	if cfg.Access.PurgeRetentionDays != config.DefaultPurgeRetentionDays {
		t.Fatalf("retention = %d, want the default %d",
			cfg.Access.PurgeRetentionDays, config.DefaultPurgeRetentionDays)
	}
}

func TestPurgeExpiredRespectsRetention(t *testing.T) {
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Access.SoftDelete = true
		c.Access.PurgeRetentionDays = 7
	})
	ctx := context.Background()
	result := upload(t, svc.Service, "recent.png", pngBytes())
	if err := svc.Manager.Delete(ctx, result.Resource.ID, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}

	mgr := svc.Manager.(*manager.DefaultManager)
	purged, err := mgr.PurgeExpired(ctx, 100)
	if err != nil {
		t.Fatalf("purge expired: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged %d records that are still inside the retention window", purged)
	}
}

func TestPurgeExpiredRefusesWithoutRetention(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mgr := svc.Manager.(*manager.DefaultManager)
	if _, err := mgr.PurgeExpired(context.Background(), 10); err == nil {
		t.Fatal("expected PurgeExpired to refuse without a configured retention window")
	}
}
