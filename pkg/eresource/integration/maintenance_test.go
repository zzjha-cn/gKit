package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// 调度器真的会按间隔触发任务，而不只是一个能被调用的方法。
func TestMaintenanceRunsScheduledPurge(t *testing.T) {
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Access.SoftDelete = true
		c.Access.PurgeRetentionDays = 7
	})
	ctx := context.Background()
	result := upload(t, svc.Service, "doomed.png", pngBytes())
	if err := svc.Manager.Delete(ctx, result.Resource.ID, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// 把软删时间推到保留期之外，让它成为清理对象。
	deleted, err := svc.Manager.Get(ctx, result.Resource.ID, nil)
	if err == nil && deleted != nil {
		t.Fatal("a soft-deleted resource should not be readable")
	}

	mgr := svc.Manager.(*manager.DefaultManager)
	maintenance := mgr.StartMaintenance(manager.MaintenanceOptions{
		PurgeExpiredEvery: 20 * time.Millisecond,
		PurgeLimit:        10,
	})
	defer maintenance.Stop()

	// 保留期是 7 天，这条记录还在窗口内，调度跑多少轮都不该清掉它。
	// 验证的是「调度器在跑且尊重保留期」，而不是「它会删东西」。
	time.Sleep(120 * time.Millisecond)
	total, err := svc.Manager.Count(ctx, &model.Query{Status: model.ResourceStatusDeleted})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Fatalf("scheduled purge removed a record still inside the retention window (count=%d)", total)
	}
}

// Stop 必须等任务真正退出，否则进程退出时还有 goroutine 在写库。
func TestMaintenanceStopIsIdempotentAndWaits(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mgr := svc.Manager.(*manager.DefaultManager)
	maintenance := mgr.StartMaintenance(manager.MaintenanceOptions{
		ReconcileUsageEvery: 10 * time.Millisecond,
	})
	time.Sleep(30 * time.Millisecond)
	maintenance.Stop()
	maintenance.Stop() // 重复调用必须安全
}

// 孤儿对账没给安全窗口时整条任务不启用——
// 宁可不跑，也不能用一个猜出来的窗口去删正在上传的文件。
func TestMaintenanceSkipsOrphanSweepWithoutSafetyWindow(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	orphan := writeOrphan(t, svc, "crm/acct/stray.bin", 72*time.Hour)

	mgr := svc.Manager.(*manager.DefaultManager)
	maintenance := mgr.StartMaintenance(manager.MaintenanceOptions{
		ReconcileOrphansEvery: 10 * time.Millisecond,
		// 刻意不给 OrphanSafetyWindow
	})
	defer maintenance.Stop()
	time.Sleep(60 * time.Millisecond)

	if _, err := os.Stat(filepath.Join(svc.root, orphan)); err != nil {
		t.Fatalf("orphan sweep ran without a safety window: %v", err)
	}
}

// 给了窗口就真的会清理。
func TestMaintenanceSweepsOrphansWhenConfigured(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	orphan := writeOrphan(t, svc, "crm/acct/stray.bin", 72*time.Hour)

	mgr := svc.Manager.(*manager.DefaultManager)
	maintenance := mgr.StartMaintenance(manager.MaintenanceOptions{
		ReconcileOrphansEvery: 10 * time.Millisecond,
		OrphanSafetyWindow:    24 * time.Hour,
	})
	defer maintenance.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(svc.root, orphan)); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scheduled orphan sweep never removed the orphan")
}

// 配了孤儿对账却漏了安全窗口，必须在启动校验时就报出来。
func TestMaintenanceConfigRequiresSafetyWindow(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Maintenance = config.MaintenanceConfig{
		Enable:                       true,
		ReconcileOrphansEveryMinutes: 60,
	}
	cfg.Normalize()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "orphan_safety_window_hours") {
		t.Fatalf("err = %v, want a safety-window validation error", err)
	}
}

// 默认不开：后台任务会扫全表、会删对象，不该由一次依赖升级悄悄开始跑。
func TestMaintenanceIsOffByDefault(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Normalize()
	if cfg.Maintenance.Enable {
		t.Fatal("maintenance must be opt-in")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// 调度器能接上这个任务。
func TestMaintenanceExpiresIdempotencyKeys(t *testing.T) {
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Manager.IdempotencyTTL = 1
	})
	ctx := context.Background()
	result := uploadIdempotent(t, svc.Service, "a.png", pngBytes(), "key-ttl")

	mgr := svc.Manager.(*manager.DefaultManager)
	maintenance := mgr.StartMaintenance(manager.MaintenanceOptions{
		ExpireIdempotencyEvery: 20 * time.Millisecond,
	})
	defer maintenance.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := svc.Manager.Get(ctx, result.Resource.ID, nil)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if current.IdempotencyKey == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the scheduled task never expired the idempotency key")
}

func TestReconcileDeletesOrphanObjects(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	ctx := context.Background()

	live := upload(t, svc.Service, "live.png", pngBytes())
	orphan := writeOrphan(t, svc, "crm/acct/stray.bin", 48*time.Hour)

	mgr := svc.Manager.(*manager.DefaultManager)
	result, err := mgr.ReconcileOrphans(ctx, &manager.ReconcileOptions{
		Before: time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Orphans != 1 || result.Deleted != 1 {
		t.Fatalf("result = %+v, want exactly one orphan deleted", result)
	}
	if _, err = os.Stat(filepath.Join(svc.root, orphan)); !os.IsNotExist(err) {
		t.Errorf("orphan %q should have been removed", orphan)
	}
	// 有记录指向的对象一个都不能碰。
	if _, err = os.Stat(filepath.Join(svc.root, live.Resource.ObjectKey)); err != nil {
		t.Errorf("a referenced object was deleted: %v", err)
	}
}

// 安全窗口是正确性的一部分：窗口内的对象可能是一次正在进行的上传
// （对象已写、记录未建），删掉它就是删掉用户正在传的文件。
func TestReconcileSkipsObjectsInsideSafetyWindow(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	fresh := writeOrphan(t, svc, "crm/acct/in-flight.bin", time.Minute)

	mgr := svc.Manager.(*manager.DefaultManager)
	result, err := mgr.ReconcileOrphans(context.Background(), &manager.ReconcileOptions{
		Before: time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Orphans != 0 {
		t.Fatalf("a fresh object was judged an orphan: %+v", result)
	}
	if _, err = os.Stat(filepath.Join(svc.root, fresh)); err != nil {
		t.Errorf("an in-flight upload was deleted: %v", err)
	}
}

// 漏传安全窗口必须直接拒绝，不能有默认值兜底——
// 这个参数写错的后果是删掉正在上传的文件。
func TestReconcileRefusesWithoutSafetyWindow(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mgr := svc.Manager.(*manager.DefaultManager)
	if _, err := mgr.ReconcileOrphans(context.Background(), &manager.ReconcileOptions{}); err == nil {
		t.Fatal("expected reconcile without a Before window to be refused")
	}
	if _, err := mgr.ReconcileOrphans(context.Background(), nil); err == nil {
		t.Fatal("expected reconcile with nil options to be refused")
	}
}

// DryRun 只报告不删除：第一次在生产上跑必须先看一眼结果。
func TestReconcileDryRunReportsWithoutDeleting(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	orphan := writeOrphan(t, svc, "crm/acct/stray.bin", 48*time.Hour)

	mgr := svc.Manager.(*manager.DefaultManager)
	result, err := mgr.ReconcileOrphans(context.Background(), &manager.ReconcileOptions{
		Before: time.Now().Add(-24 * time.Hour),
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Orphans != 1 {
		t.Fatalf("dry run should still report the orphan: %+v", result)
	}
	if result.Deleted != 0 {
		t.Errorf("dry run deleted %d objects", result.Deleted)
	}
	if _, err = os.Stat(filepath.Join(svc.root, orphan)); err != nil {
		t.Errorf("dry run removed the object: %v", err)
	}
}

// 软删的记录仍然引用着对象，保留期内绝不能被孤儿清理顺手删掉——
// 否则「可恢复」就是一句谎话。
func TestReconcileKeepsObjectsOfSoftDeletedRecords(t *testing.T) {
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Access.SoftDelete = true
	})
	ctx := context.Background()
	result := upload(t, svc.Service, "doomed.png", pngBytes())
	if err := svc.Manager.Delete(ctx, result.Resource.ID, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// 把对象的时间推到安全窗口之外，让它成为对账的候选。
	full := filepath.Join(svc.root, result.Resource.ObjectKey)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	mgr := svc.Manager.(*manager.DefaultManager)
	reconciled, err := mgr.ReconcileOrphans(ctx, &manager.ReconcileOptions{
		Before: time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reconciled.Deleted != 0 {
		t.Fatalf("a soft-deleted record's object was purged by the orphan sweep: %+v", reconciled)
	}
	if _, err = os.Stat(full); err != nil {
		t.Errorf("object of a soft-deleted record is gone: %v", err)
	}
}

// 反向对账只报告不删记录：那条记录是误删或 bug 的唯一证据。
func TestReconcileMissingReportsWithoutDeleting(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	ctx := context.Background()
	result := upload(t, svc.Service, "vanished.png", pngBytes())
	if err := os.Remove(filepath.Join(svc.root, result.Resource.ObjectKey)); err != nil {
		t.Fatalf("remove object: %v", err)
	}

	mgr := svc.Manager.(*manager.DefaultManager)
	missing, err := mgr.ReconcileMissing(ctx, &model.Query{})
	if err != nil {
		t.Fatalf("reconcile missing: %v", err)
	}
	if len(missing) != 1 || missing[0] != result.Resource.ID {
		t.Fatalf("missing = %v, want [%s]", missing, result.Resource.ID)
	}
	if _, err = svc.Manager.Get(ctx, result.Resource.ID, nil); err != nil {
		t.Errorf("the record was deleted by the reverse sweep: %v", err)
	}
}

// 回流必须能被调度起来，而不只是一个能手调的方法。
func TestMaintenanceIngestsEdgeLogsOnSchedule(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mgr := svc.Manager.(*manager.DefaultManager)
	fetcher := &countingLogFetcher{}
	mgr.SetCDNLogFetcher(fetcher)

	maintenance := mgr.StartMaintenance(manager.MaintenanceOptions{
		EdgeLogEvery:           20 * time.Millisecond,
		EdgeLogDomain:          "cdn.test",
		EdgeLogLag:             time.Millisecond,
		EdgeLogInitialLookback: time.Hour,
	})
	defer maintenance.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fetcher.windows()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	windows := fetcher.windows()
	if len(windows) < 2 {
		t.Fatalf("scheduled ingest ran %d times, want at least 2", len(windows))
	}
	// 游标必须推进：第二轮的起点应当是第一轮的终点，
	// 否则每轮都在重捞同一段，统计会越滚越高。
	if !windows[1][0].Equal(windows[0][1]) {
		t.Errorf("window 2 starts at %v, want it to resume from %v", windows[1][0], windows[0][1])
	}
	if maintenance.EdgeCursor().IsZero() {
		t.Error("cursor was never advanced")
	}
}

// 失败时不能推进游标——宁可重复统计，也不要留一段永远补不上的空洞。
func TestMaintenanceDoesNotAdvanceCursorOnFailure(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mgr := svc.Manager.(*manager.DefaultManager)
	fetcher := &countingLogFetcher{failing: true}
	mgr.SetCDNLogFetcher(fetcher)

	maintenance := mgr.StartMaintenance(manager.MaintenanceOptions{
		EdgeLogEvery:           20 * time.Millisecond,
		EdgeLogDomain:          "cdn.test",
		EdgeLogLag:             time.Millisecond,
		EdgeLogInitialLookback: time.Hour,
	})
	defer maintenance.Stop()

	before := maintenance.EdgeCursor()
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if len(fetcher.windows()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	windows := fetcher.windows()
	if len(windows) < 2 {
		t.Fatalf("ran %d times, want at least 2", len(windows))
	}
	// 两轮的起点必须相同：失败那轮不该把区间吃掉。
	if !windows[1][0].Equal(windows[0][0]) {
		t.Errorf("a failed ingest advanced the window from %v to %v", windows[0][0], windows[1][0])
	}
	if !maintenance.EdgeCursor().Equal(before) {
		t.Error("cursor moved despite every ingest failing")
	}
}

// 模拟「provider.Upload 成功、store.Create 之前进程崩溃」：
// 直接往存储根目录里塞一个没有任何记录指向的文件。
func writeOrphan(t *testing.T, svc *serviceWithRoot, name string, age time.Duration) string {
	t.Helper()
	full := filepath.Join(svc.root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte("orphan"), 0o644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return name
}
