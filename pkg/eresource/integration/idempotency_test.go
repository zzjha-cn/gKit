package integration_test

import (
	"bytes"
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// 上传时就要把失效时间写进去，否则清理任务永远找不到该清谁。
func TestIdempotencyKeyGetsExpiry(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	result := uploadIdempotent(t, svc.Service, "a.png", pngBytes(), "key-ttl")
	if result.Resource.IdempotencyExpireAt.IsZero() {
		t.Fatal("an idempotency key was stored without an expiry")
	}
	want := time.Now().Add(time.Duration(config.DefaultIdempotencyTTL) * time.Second)
	if diff := result.Resource.IdempotencyExpireAt.Sub(want); diff > time.Minute || diff < -time.Minute {
		t.Errorf("expiry = %v, want roughly %v", result.Resource.IdempotencyExpireAt, want)
	}
	// 不带键的上传不该凭空长出一个过期时间。
	plain := upload(t, svc.Service, "b.png", pngBytes())
	if !plain.Resource.IdempotencyExpireAt.IsZero() {
		t.Error("an upload without an idempotency key got an expiry anyway")
	}
}

// 过期清理摘掉的是**键**，记录必须留着。
func TestExpireIdempotencyKeysClearsKeyNotRecord(t *testing.T) {
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Manager.IdempotencyTTL = 1 // 1 秒，立刻就能过期
	})
	ctx := context.Background()
	result := uploadIdempotent(t, svc.Service, "a.png", pngBytes(), "key-ttl")
	id := result.Resource.ID

	time.Sleep(1100 * time.Millisecond)

	mgr := svc.Manager.(*manager.DefaultManager)
	cleared, err := mgr.ExpireIdempotencyKeys(ctx, 100)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("cleared = %d, want 1", cleared)
	}
	after, err := svc.Manager.Get(ctx, id, nil)
	if err != nil {
		t.Fatalf("the resource itself must survive key expiry: %v", err)
	}
	if after.IdempotencyKey != "" {
		t.Errorf("idempotency key = %q, want it cleared", after.IdempotencyKey)
	}
	if !after.IdempotencyExpireAt.IsZero() {
		t.Errorf("expiry = %v, want it cleared too", after.IdempotencyExpireAt)
	}
}

// 键过期之后，同一个键的新上传应当产生一条**新记录**——
// 过期的意思就是"这次重试不再命中"。
func TestExpiredKeyNoLongerCollapsesRetries(t *testing.T) {
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Manager.IdempotencyTTL = 1
	})
	ctx := context.Background()
	first := uploadIdempotent(t, svc.Service, "a.png", pngBytes(), "key-ttl")

	time.Sleep(1100 * time.Millisecond)
	mgr := svc.Manager.(*manager.DefaultManager)
	if _, err := mgr.ExpireIdempotencyKeys(ctx, 100); err != nil {
		t.Fatalf("expire: %v", err)
	}

	second := uploadIdempotent(t, svc.Service, "a.png", pngBytes(), "key-ttl")
	if second.Resource.ID == first.Resource.ID {
		t.Fatal("an expired key still collapsed a new upload onto the old record")
	}
	if !second.Uploaded {
		t.Error("the new upload should have written its own object")
	}
}

// 还没到期的键不能被提前清掉——清早了，客户端的重试就会产生第二条记录。
func TestExpireIdempotencyKeysSkipsLiveKeys(t *testing.T) {
	svc := newServiceWithRoot(t, nil) // 默认 TTL 24h
	ctx := context.Background()
	uploadIdempotent(t, svc.Service, "a.png", pngBytes(), "key-live")

	mgr := svc.Manager.(*manager.DefaultManager)
	cleared, err := mgr.ExpireIdempotencyKeys(ctx, 100)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if cleared != 0 {
		t.Fatalf("cleared %d live keys", cleared)
	}
}

// 不带幂等键的记录绝不能被扫进清理批次——绝大多数上传都不带键，
// 少了 $exists 条件的话每一轮都会把它们全捞出来。
func TestExpireIdempotencyKeysIgnoresRecordsWithoutKeys(t *testing.T) {
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Manager.IdempotencyTTL = 1
	})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		upload(t, svc.Service, "plain-"+strconv.Itoa(i)+".png", pngBytes())
	}
	time.Sleep(1100 * time.Millisecond)

	mgr := svc.Manager.(*manager.DefaultManager)
	cleared, err := mgr.ExpireIdempotencyKeys(ctx, 100)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if cleared != 0 {
		t.Fatalf("cleared = %d, want 0 — records without keys must not be scanned", cleared)
	}
}

// 清键不能把 updated_at 推到当下：软删的保留期是按它算的，
// 刷新一次就等于让那条记录在回收站里重新待满一个周期。
func TestExpiringKeyDoesNotRefreshUpdatedAt(t *testing.T) {
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Manager.IdempotencyTTL = 1
	})
	ctx := context.Background()
	result := uploadIdempotent(t, svc.Service, "a.png", pngBytes(), "key-ttl")
	before := result.Resource.UpdatedAt

	time.Sleep(1100 * time.Millisecond)
	mgr := svc.Manager.(*manager.DefaultManager)
	if _, err := mgr.ExpireIdempotencyKeys(ctx, 100); err != nil {
		t.Fatalf("expire: %v", err)
	}
	after, err := svc.Manager.Get(ctx, result.Resource.ID, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !after.UpdatedAt.Equal(before) {
		t.Fatalf("updated_at moved from %v to %v; the soft-delete retention clock would be reset",
			before, after.UpdatedAt)
	}
}

// 同一个幂等键重复提交：一条记录、一个对象，第二次不写任何字节。
func TestIdempotencyKeyCollapsesRetries(t *testing.T) {
	svc := newTestService(t, nil)
	body := pngBytes()

	first := uploadIdempotent(t, svc, "retry.png", body, "key-1")
	second := uploadIdempotent(t, svc, "retry.png", body, "key-1")

	if first.Resource.ID != second.Resource.ID {
		t.Fatalf("retry produced a new record: %s vs %s", first.Resource.ID, second.Resource.ID)
	}
	if second.Uploaded {
		t.Error("retry should not have written any bytes")
	}
	if !second.Existed {
		t.Error("retry should report Existed")
	}
	if second.URL == "" {
		t.Error("retry should still hand back a usable url")
	}

	total, err := svc.Manager.Count(context.Background(), &model.Query{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Fatalf("record count = %d, want 1", total)
	}
}

// 幂等键只在租户内生效：键由调用方自己生成，跨租户撞车完全可能，
// 跨租户命中就是一次越权读。
func TestIdempotencyKeyIsTenantScoped(t *testing.T) {
	svc := newTestService(t, nil)
	ctx := context.Background()
	body := pngBytes()

	mk := func(ns, account string) *model.UploadResult {
		result, err := svc.Manager.Upload(ctx, &model.UploadRequest{
			FileName:       "shared-key.png",
			Reader:         bytes.NewReader(body),
			BizType:        "image",
			Namespace:      ns,
			AccountID:      account,
			IdempotencyKey: "same-key",
		})
		if err != nil {
			t.Fatalf("upload for %s/%s: %v", ns, account, err)
		}
		return result
	}
	a := mk("crm", "acct-a")
	b := mk("crm", "acct-b")

	if a.Resource.ID == b.Resource.ID {
		t.Fatal("idempotency key leaked across tenants")
	}
	if !b.Uploaded {
		t.Error("the other tenant's upload should have written its own object")
	}
}

// 不带幂等键的上传互不影响——稀疏索引的语义必须是「没有键就不参与唯一性」。
func TestUploadsWithoutIdempotencyKeyAreIndependent(t *testing.T) {
	svc := newTestService(t, nil)
	first := upload(t, svc, "a.png", pngBytes())
	second := upload(t, svc, "b.png", pngBytes())
	if first.Resource.ID == second.Resource.ID {
		t.Fatal("uploads without an idempotency key should never collapse")
	}
}

func uploadIdempotent(t *testing.T, svc *eresource.Service, name string, body []byte, key string) *model.UploadResult {
	t.Helper()
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName:       name,
		Reader:         bytes.NewReader(body),
		BizType:        "image",
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	return result
}
