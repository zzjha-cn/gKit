package integration_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
	"github.com/zzjha-cn/gKit/pkg/eresource/store/jsonstore"
)

// 频控必须排在验签之后：否则任何人都能拿无效签名把某个 object 的配额刷满，
// 把限流本身变成针对合法用户的拒绝服务手段。
func TestInvalidSignatureDoesNotConsumeRateLimitQuota(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
		cfg.Access.RateLimitByResource = 1
		cfg.Access.RateLimitWindowSeconds = 60
	})
	router := newTestRouter(t, svc)
	res := uploadForTenant(t, svc, "crm", "acct-a", "alice")

	signed, err := svc.Manager.GetAccessURL(context.Background(), res.ID, nil)
	if err != nil {
		t.Fatalf("build signed url: %v", err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}

	// 攻击者反复用伪造签名打同一个 object
	forged := parsed.Query()
	forged.Set(provider.SignParamSign, strings.Repeat("0", 64))
	for i := 0; i < 5; i++ {
		rec := serve(router, httptest.NewRequest(http.MethodGet, parsed.Path+"?"+forged.Encode(), nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("伪造签名第 %d 次 status = %d，期望 403", i+1, rec.Code)
		}
	}

	// 合法持有者的配额不应该被消耗掉
	rec := serve(router, httptest.NewRequest(http.MethodGet, parsed.Path+"?"+parsed.RawQuery, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("合法签名下载 status = %d，期望 200——"+
			"无效签名消耗了配额，限流被反过来当成了拒绝服务手段", rec.Code)
	}
}

// driftingQuota 模拟增量计数漂移：AddUsage 全部丢掉，
// 等价于一连串 $inc 失败或进程在 Create 与 AddUsage 之间崩溃。
type driftingQuota struct {
	store.Store
	counted model.Usage
}

func newDriftingService(t *testing.T) (*eresource.Service, *driftingQuota) {
	t.Helper()
	dir := t.TempDir()
	cfg := baseConfig(dir)
	base, err := jsonstore.NewJSONStore(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	quota := &driftingQuota{Store: base}
	svc, err := eresource.New(cfg, eresource.WithStore(quota))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc, quota
}

// 账面漂了之后，对账必须能发现并修正——否则偏差只累积不自愈，
// 而配额正是拿这个数去拦上传的。
func TestReconcileUsageDetectsAndCorrectsDrift(t *testing.T) {
	svc, quota := newDriftingService(t)
	ctx := context.Background()
	body := pngBytes()
	upload(t, svc, "a.png", body)

	// 人为把账面改歪，模拟一次丢失的 $inc。
	quota.counted.Bytes = 0
	quota.counted.Files = 0

	mgr := svc.Manager.(*manager.DefaultManager)
	drift, err := mgr.ReconcileUsage(ctx, "", "", false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !drift.HasDrift() {
		t.Fatal("reconcile did not notice the drift")
	}
	if drift.BytesDiff != int64(len(body)) || drift.FilesDiff != 1 {
		t.Fatalf("drift = %+d bytes / %+d files, want %+d / +1", drift.BytesDiff, drift.FilesDiff, len(body))
	}
	// correct=false 时只报告，不动账面：偏差巨大往往意味着别处出了事故，
	// 这时候直接把账面改成"真实值"等于把现场也抹平。
	if drift.Corrected {
		t.Error("a report-only reconcile modified the counter")
	}
	if quota.counted.Files != 0 {
		t.Error("a report-only reconcile should not have touched the counter")
	}

	if drift, err = mgr.ReconcileUsage(ctx, "", "", true); err != nil {
		t.Fatalf("reconcile with correct: %v", err)
	}
	if !drift.Corrected {
		t.Fatal("reconcile did not correct the counter")
	}
	after, err := svc.Manager.Usage(ctx, "", "")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if after.Bytes != int64(len(body)) || after.Files != 1 {
		t.Fatalf("usage after correction = %+v, want %d bytes / 1 file", after, len(body))
	}
}

// 没有漂移时对账不应该改动任何东西。
func TestReconcileUsageIsNoopWhenAccurate(t *testing.T) {
	svc, _ := newDriftingService(t)
	ctx := context.Background()
	upload(t, svc, "a.png", pngBytes())

	mgr := svc.Manager.(*manager.DefaultManager)
	drift, err := mgr.ReconcileUsage(ctx, "", "", true)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if drift.HasDrift() || drift.Corrected {
		t.Fatalf("drift = %+v, want none", drift)
	}
}

// 全量对账要能自己扫出租户清单——模块没有租户表，也不该有。
func TestReconcileAllUsageCoversEveryTenant(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	ctx := context.Background()
	uploadForTenant(t, svc.Service, "crm", "acct-a", "u-1")
	uploadForTenant(t, svc.Service, "crm", "acct-b", "u-2")

	mgr := svc.Manager.(*manager.DefaultManager)
	// JSONStore 的账面与现算同源，不会有漂移；这里验证的是
	// 对账能遍历到每个租户而不是只看一个。
	drifts, err := mgr.ReconcileAllUsage(ctx, false)
	if err != nil {
		t.Fatalf("reconcile all: %v", err)
	}
	if len(drifts) != 0 {
		t.Fatalf("unexpected drift: %+v", drifts)
	}
}

// 采样必须是**确定性**的：同一个对象要么一直被采、要么一直不被采。
// 随机采样下，一个被读 1000 次的热点和 1000 个各读一次的冷门对象
// 会采出相同的样本分布，而区分这两者正是这个指标的全部意义。
func TestObjectReadSamplingIsDeterministic(t *testing.T) {
	metrics := newRecordingMetrics()
	svc := newServiceWithRoot(t, func(c *config.Config) {
		// 全量采集，先确认热点对象每次都被记到。
		c.Manager.ObjectReadSampleRate = 1
	}, metrics)
	result := upload(t, svc.Service, "hot.png", pngBytes())

	router := newTestRouter(t, svc.Service)
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/resource/"+result.Resource.ID+"/content", nil)
		if rec := serve(router, req); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}
	if got := metrics.get(manager.MetricObjectRead); got != 5 {
		t.Fatalf("%s = %d, want 5 at full sampling", manager.MetricObjectRead, got)
	}
	// 下载总数不采样：成本分布要的是精确值。
	if got := metrics.get(manager.MetricDownloadTotal); got != 5 {
		t.Errorf("%s = %d, want 5 — download counters must never be sampled",
			manager.MetricDownloadTotal, got)
	}
}

// 采样率设成"不可能命中"时，读放大指标一条都不该出现，
// 但下载总量与字节数必须照常统计——它们是成本账,不能采样。
func TestObjectReadSamplingCanBeDisabled(t *testing.T) {
	metrics := newRecordingMetrics()
	svc := newServiceWithRoot(t, func(c *config.Config) {
		c.Manager.ObjectReadSampleRate = -1 // 显式关闭
	}, metrics)
	result := upload(t, svc.Service, "cold.png", pngBytes())

	router := newTestRouter(t, svc.Service)
	req := httptest.NewRequest(http.MethodGet, "/resource/"+result.Resource.ID+"/content", nil)
	if rec := serve(router, req); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := metrics.get(manager.MetricObjectRead); got != 0 {
		t.Errorf("%s = %d, want 0 when sampling is off", manager.MetricObjectRead, got)
	}
	if got := metrics.get(manager.MetricDownloadBytes); got == 0 {
		t.Error("download bytes must still be counted when object-key sampling is off")
	}
}

// 默认就该是采样而不是全量：object key 的基数等于对象总数，
// 全量打点会在监控后端造出百万级 label 的时间序列。
func TestObjectReadSamplingDefaultsToOnePercent(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Normalize()
	if cfg.Manager.ObjectReadSampleRate != config.DefaultObjectReadSampleRate {
		t.Fatalf("sample rate = %d, want the default %d",
			cfg.Manager.ObjectReadSampleRate, config.DefaultObjectReadSampleRate)
	}
	if config.DefaultObjectReadSampleRate <= 1 {
		t.Fatal("the default must actually sample, not collect every object key")
	}
}

// 1% 采样下，大量不同的 object key 只有少数被采到——
// 这是高基数标签不会压垮监控后端的保证。
func TestObjectReadSamplingKeepsCardinalityLow(t *testing.T) {
	sampled := 0
	const total = 2000
	for i := 0; i < total; i++ {
		if manager.SampleObjectKeyForTest("crm/acct/obj-"+strconv.Itoa(i)+".bin",
			config.DefaultObjectReadSampleRate) {
			sampled++
		}
	}
	if sampled == 0 {
		t.Fatal("1% sampling never fired across 2000 keys")
	}
	// 宽松上界：只要远低于全量即可，不去断言精确的 1%。
	if sampled > total/10 {
		t.Fatalf("sampled %d of %d keys, far above the intended 1%%", sampled, total)
	}
}

// 口径：按对象计量。去重让两条记录共享一个对象时，用量只算一次。
func TestUsageCountsObjectsNotRecords(t *testing.T) {
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.EnableDedup = true
		c.Manager.DedupStrategy = config.DedupStrategyNewRecord
	})
	ctx := context.Background()
	body := pngBytes()

	first := upload(t, svc, "same.png", body)
	second := upload(t, svc, "same.png", body)
	if first.Resource.ObjectKey != second.Resource.ObjectKey {
		t.Fatalf("dedup did not share the object: %q vs %q", first.Resource.ObjectKey, second.Resource.ObjectKey)
	}
	if first.Resource.ID == second.Resource.ID {
		t.Fatal("new_record strategy should mint a second record")
	}

	usage, err := svc.Manager.Usage(ctx, "", "")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage == nil {
		t.Fatal("usage should be available with the bundled store")
	}
	if usage.Files != 1 {
		t.Errorf("usage.Files = %d, want 1 (two records share one object)", usage.Files)
	}
	if usage.Bytes != int64(len(body)) {
		t.Errorf("usage.Bytes = %d, want %d", usage.Bytes, len(body))
	}
}

func TestQuotaRejectsUploadOverLimit(t *testing.T) {
	body := pngBytes()
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.EnableQuota = true
		c.Manager.QuotaMaxBytes = int64(len(body)) + 1
	})
	upload(t, svc, "first.png", body)

	_, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "second.png",
		Reader:   bytes.NewReader(body),
		BizType:  "image",
	})
	if err == nil {
		t.Fatal("expected the second upload to exceed the quota")
	}
	if !strings.Contains(err.Error(), "quota") {
		t.Fatalf("error = %v, want a quota error", err)
	}
}

// 配额关闭时照常记账，只是不拦截——先有数字再谈限额。
func TestUsageIsTrackedEvenWhenQuotaIsOff(t *testing.T) {
	svc := newTestService(t, nil)
	body := pngBytes()
	upload(t, svc, "tracked.png", body)

	usage, err := svc.Manager.Usage(context.Background(), "", "")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage == nil || usage.Files != 1 || usage.Bytes != int64(len(body)) {
		t.Fatalf("usage = %+v, want 1 file of %d bytes", usage, len(body))
	}
}

func (d *driftingQuota) AddUsage(_ context.Context, _, _ string, bytes, files int64) error {
	d.counted.Bytes += bytes
	d.counted.Files += files
	return nil
}

func (d *driftingQuota) GetUsage(_ context.Context, ns, acc string) (*model.Usage, error) {
	usage := d.counted
	usage.Namespace, usage.AccountID = ns, acc
	return &usage, nil
}

func (d *driftingQuota) ComputeUsage(ctx context.Context, ns, acc string) (*model.Usage, error) {
	// 真实值从底层 Store 现算——JSONStore 的实现就是按 object key 去重累加。
	return d.Store.(store.QuotaStore).ComputeUsage(ctx, ns, acc)
}
