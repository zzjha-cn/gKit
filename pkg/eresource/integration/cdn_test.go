package integration_test

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/cdn"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// 切了 CDN 鉴权却不开对齐，等于白切——这条必须提醒。
func TestWarnsAboutCDNAuthWithoutAlignment(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Access.URLMode = config.URLModeCDNAuthA
	cfg.Access.CDNAuthKey = "k"
	cfg.Normalize()
	warnings := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(warnings, "signed_url_align_sec") {
		t.Fatalf("warnings = %q, want an alignment warning", warnings)
	}
}

// CDN 鉴权模式下 protected 走边缘验签，形态稳定可缓存。
func TestCDNAuthModeSignsProtected(t *testing.T) {
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
		c.Access.URLMode = config.URLModeCDNAuthA
		c.Access.CDNAuthKey = "cdn-secret"
		c.Access.CDNDomain = "edge.example.com"
		c.Access.SignedURLAlignSec = 3600
	})
	result := upload(t, svc, "poster.png", pngBytes())
	signed, err := svc.Manager.GetAccessURL(context.Background(), result.Resource.ID, nil)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !strings.Contains(signed, "edge.example.com") {
		t.Errorf("cdn url = %q, want the cdn domain", signed)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	authKey := parsed.Query().Get("auth_key")
	if authKey == "" {
		t.Fatalf("cdn url is missing auth_key: %s", signed)
	}
	// ts-rand-uid-md5：rand 固定成 0 正是为了让地址可缓存。
	parts := strings.Split(authKey, "-")
	if len(parts) != 4 {
		t.Fatalf("auth_key = %q, want 4 dash-separated fields", authKey)
	}
	if parts[1] != "0" {
		t.Errorf("auth_key rand = %q, want a fixed 0 so the cache key stays stable", parts[1])
	}
	if len(parts[3]) != 32 {
		t.Errorf("auth_key digest = %q, want a 32-char md5", parts[3])
	}
}

// 这是 2.4 的硬约束：private 永远不走 CDN 鉴权，哪怕配置就是那么写的。
// 放开它等于悄悄把 private 降级成可转发的 bearer token。
func TestPrivateNeverUsesCDNAuth(t *testing.T) {
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
		c.Access.URLMode = config.URLModeCDNAuthA
		c.Access.CDNAuthKey = "cdn-secret"
		c.Access.CDNDomain = "edge.example.com"
	})
	result := upload(t, svc, "contract.png", pngBytes())
	signed, err := svc.Manager.GetAccessURL(context.Background(), result.Resource.ID,
		&model.AccessRequest{UserID: "u-1"})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if strings.Contains(signed, "auth_key=") {
		t.Fatalf("private resource was signed with cdn auth: %s", signed)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Query().Get("s") == "" {
		t.Fatalf("private url should fall back to the provider signature: %s", signed)
	}
}

// 缺密钥的 CDN 模式必须在启动时就报错，而不是签出一串谁都验不过的地址——
// 验签失败发生在边缘，业务侧只会看到客户端报 403。
func TestCDNAuthRequiresKey(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Access.URLMode = config.URLModeCDNAuthA
	if _, err := eresource.New(cfg); err == nil {
		t.Fatal("expected cdn_auth_a without cdn_auth_key to fail validation")
	}
}

// B / C 型把鉴权信息放进路径而不是查询串。
//
// 这里断言的是**形态与稳定性**，不是签名值本身：真正的向量测试要对着
// 阿里云当时的鉴权文档钉死，在没有核对过文档之前，用同一段逻辑
// 反算一遍期望值只是把实现抄了两遍，测不出拼错连接符这类错误。
func TestCDNAuthPathModes(t *testing.T) {
	cases := []struct {
		mode    string
		segment func(parts []string) bool
	}{
		{config.URLModeCDNAuthB, func(parts []string) bool { return len(parts[0]) == 12 && len(parts[1]) == 32 }},
		{config.URLModeCDNAuthC, func(parts []string) bool { return len(parts[0]) == 32 && len(parts[1]) > 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			svc := newTestService(t, func(c *config.Config) {
				c.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
				c.Access.URLMode = tc.mode
				c.Access.CDNAuthKey = "cdn-secret"
				c.Access.CDNDomain = "edge.example.com"
				c.Access.SignedURLAlignSec = 3600
			})
			result := upload(t, svc, "poster.png", pngBytes())
			signed, err := svc.Manager.GetAccessURL(context.Background(), result.Resource.ID, nil)
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			parsed, err := url.Parse(signed)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if parsed.RawQuery != "" {
				t.Errorf("path-based cdn auth should not add query params: %s", signed)
			}
			parts := strings.SplitN(strings.TrimPrefix(parsed.EscapedPath(), "/"), "/", 3)
			if len(parts) < 3 {
				t.Fatalf("path = %q, want /<a>/<b>/<objectkey>", parsed.EscapedPath())
			}
			if !tc.segment(parts) {
				t.Errorf("auth segments %q / %q have the wrong shape", parts[0], parts[1])
			}
			// 对象 key 必须完整保留在尾部，否则 CDN 回源会找不到对象。
			if !strings.Contains(signed, result.Resource.ObjectKey) {
				t.Errorf("url %q lost the object key %q", signed, result.Resource.ObjectKey)
			}
			again, err := svc.Manager.GetAccessURL(context.Background(), result.Resource.ID, nil)
			if err != nil {
				t.Fatalf("second sign: %v", err)
			}
			if again != signed {
				t.Errorf("aligned url is not stable:\n  %s\n  %s", signed, again)
			}
		})
	}
}

type recordingRefresher struct {
	mu        sync.Mutex
	refreshed []string
	preloaded []string
}

func (r *recordingRefresher) Refresh(_ context.Context, urls []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshed = append(r.refreshed, urls...)
	return nil
}

func (r *recordingRefresher) Preload(_ context.Context, urls []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.preloaded = append(r.preloaded, urls...)
	return nil
}

// 预热与刷新是相反的操作，必须分别可调——
// 大促素材要的是"在用户来之前推到边缘"，不是"让边缘失效"。
func TestPreloadCDNPushesPublicURLs(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "campaign.png", pngBytes())

	refresher := &recordingRefresher{}
	mgr := svc.Manager.(*manager.DefaultManager)
	mgr.SetCDNRefresher(refresher)

	if err := mgr.PreloadCDN(context.Background(), []string{result.Resource.ID}, nil); err != nil {
		t.Fatalf("preload: %v", err)
	}
	refresher.mu.Lock()
	defer refresher.mu.Unlock()
	if len(refresher.preloaded) != 1 {
		t.Fatalf("preloaded %d urls, want 1", len(refresher.preloaded))
	}
	if !strings.Contains(refresher.preloaded[0], result.Resource.ObjectKey) {
		t.Errorf("preloaded %q, want the object's public url", refresher.preloaded[0])
	}
	if len(refresher.refreshed) != 0 {
		t.Error("preload must not also refresh — they are opposite operations")
	}
}

func TestPreloadCDNRequiresImplementation(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "a.png", pngBytes())
	mgr := svc.Manager.(*manager.DefaultManager)
	if err := mgr.PreloadCDN(context.Background(), []string{result.Resource.ID}, nil); !errors.Is(err, manager.ErrCDNRefresherMissing) {
		t.Fatalf("err = %v, want ErrCDNRefresherMissing", err)
	}
}

// 预热同样受租户边界约束。
func TestPreloadCDNIsTenantScoped(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadForTenant(t, svc, "crm", "acct-a", "u-1")
	mgr := svc.Manager.(*manager.DefaultManager)
	mgr.SetCDNRefresher(&recordingRefresher{})
	err := mgr.PreloadCDN(context.Background(), []string{resource.ID},
		&model.AccessRequest{Namespace: "crm", AccountID: "acct-b"})
	if err == nil {
		t.Fatal("cross-tenant preload should be rejected")
	}
}

type fakeLogFetcher struct {
	files []cdn.EdgeLogFile
	body  string
}

func (f *fakeLogFetcher) ListEdgeLogs(context.Context, string, time.Time, time.Time) ([]cdn.EdgeLogFile, error) {
	return f.files, nil
}

func (f *fakeLogFetcher) OpenEdgeLog(context.Context, cdn.EdgeLogFile) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), nil
}

// 这是整条回流链路的要点：切了 CDN 之后请求不到业务服务，
// 下载量只能从边缘日志里补回来。
func TestIngestEdgeLogsBackfillsDownloadMetrics(t *testing.T) {
	metrics := newRecordingMetrics()
	audit := &capturingAudit{}
	svc := newServiceWithRoot(t, nil, metrics)
	mgr := svc.Manager.(*manager.DefaultManager)
	mgr.SetAuditSink(audit)

	result := upload(t, svc.Service, "banner.png", pngBytes())
	key := result.Resource.ObjectKey

	// 两行命中本模块的资源，一行是同域名下别的业务的请求。
	body := strings.Join([]string{
		`[12/Oct/2026:10:00:00 +0800] 1.2.3.4 - 100 "-" "GET http://cdn.test/` + key + ` HTTP/1.1" 200 2048 "-" "ua" HIT`,
		`[12/Oct/2026:10:00:01 +0800] 1.2.3.5 - 100 "-" "GET http://cdn.test/` + key + ` HTTP/1.1" 200 1024 "-" "ua" MISS`,
		`[12/Oct/2026:10:00:02 +0800] 1.2.3.6 - 100 "-" "GET http://cdn.test/other/app.js HTTP/1.1" 200 512 "-" "ua" HIT`,
		`[12/Oct/2026:10:00:03 +0800] 1.2.3.7 - 100 "-" "GET http://cdn.test/` + key + ` HTTP/1.1" 404 0 "-" "ua" MISS`,
	}, "\n")
	mgr.SetCDNLogFetcher(&fakeLogFetcher{
		files: []cdn.EdgeLogFile{{Name: "log-1"}},
		body:  body,
	})

	res, err := mgr.IngestEdgeLogs(context.Background(), "cdn.test",
		time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Files != 1 {
		t.Fatalf("files = %d, want 1", res.Files)
	}
	if res.Matched != 2 {
		t.Fatalf("matched = %d, want 2 (the third line is another business, the fourth is a 404)", res.Matched)
	}
	// 404 不消耗出口带宽，不该算进下载量。
	if res.Bytes != 2048+1024+512 {
		t.Errorf("bytes = %d, want only the successful responses counted", res.Bytes)
	}
	if res.CacheHits != 2 {
		t.Errorf("cache hits = %d, want 2", res.CacheHits)
	}
	// 指标补上了，而且标在 edge 维度而不是混进 proxy。
	if got := metrics.get(manager.MetricDownloadTotal); got != 2 {
		t.Errorf("%s = %d, want 2", manager.MetricDownloadTotal, got)
	}
	if got := metrics.label(manager.MetricDownloadTotal, "route"); got != "edge" {
		t.Errorf("route label = %q, want edge — backfilled data must stay distinguishable", got)
	}
	// 审计也补上了：走 CDN 的那部分流量在实时链路上一条记录都没有。
	events := audit.all()
	if len(events) != 2 {
		t.Fatalf("recorded %d audit events, want 2", len(events))
	}
	if events[0].Route != "edge" || events[0].ObjectKey != key {
		t.Errorf("unexpected audit event: %+v", events[0])
	}
}

func TestIngestEdgeLogsRequiresFetcher(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mgr := svc.Manager.(*manager.DefaultManager)
	_, err := mgr.IngestEdgeLogs(context.Background(), "cdn.test", time.Now().Add(-time.Hour), time.Now())
	if !errors.Is(err, manager.ErrEdgeLogUnsupported) {
		t.Fatalf("err = %v, want ErrEdgeLogUnsupported", err)
	}
}

// 解析必须宽容：厂商改一次日志格式就让整条回流链路报错停摆，
// 代价远大于丢几行统计。
func TestEdgeLogParserSkipsUnparseableLines(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
	}{
		{"empty", "", false},
		{"garbage", "this is not a log line", false},
		{"truncated", `[12/Oct/2026:10:00:00 +0800] 1.2.3.4`, false},
		{
			"normal",
			`[12/Oct/2026:10:00:00 +0800] 1.2.3.4 - 100 "-" "GET http://cdn.test/a/b.png HTTP/1.1" 200 2048 "-" "ua" HIT`,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := cdn.ParseEdgeAccessLog(tc.line)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if entry.Path != "/a/b.png" {
				t.Errorf("path = %q, want /a/b.png", entry.Path)
			}
			if entry.StatusCode != 200 || entry.Bytes != 2048 {
				t.Errorf("status/bytes = %d/%d, want 200/2048", entry.StatusCode, entry.Bytes)
			}
			if !entry.Hit {
				t.Error("hit = false, want true")
			}
		})
	}
}

type countingLogFetcher struct {
	mu      sync.Mutex
	calls   [][2]time.Time
	body    string
	failing bool
}

func (f *countingLogFetcher) ListEdgeLogs(_ context.Context, _ string, start, end time.Time) ([]cdn.EdgeLogFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]time.Time{start, end})
	if f.failing {
		return nil, errors.New("vendor api is down")
	}
	return []cdn.EdgeLogFile{{Name: "log-1"}}, nil
}

func (f *countingLogFetcher) OpenEdgeLog(context.Context, cdn.EdgeLogFile) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func (f *countingLogFetcher) windows() [][2]time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]time.Time(nil), f.calls...)
}

// 回捞窗口的右边界要向前退一个 lag：厂商日志有生成延迟，
// 捞"刚刚过去的那一分钟"只会捞到空文件，而游标却已经推过去了。
func TestEdgeLogWindowRespectsVendorLag(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mgr := svc.Manager.(*manager.DefaultManager)
	fetcher := &countingLogFetcher{}
	mgr.SetCDNLogFetcher(fetcher)

	const lag = 45 * time.Minute
	maintenance := mgr.StartMaintenance(manager.MaintenanceOptions{
		EdgeLogEvery:           20 * time.Millisecond,
		EdgeLogDomain:          "cdn.test",
		EdgeLogLag:             lag,
		EdgeLogInitialLookback: time.Hour,
	})
	defer maintenance.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fetcher.windows()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	windows := fetcher.windows()
	if len(windows) == 0 {
		t.Fatal("ingest never ran")
	}
	if gap := time.Since(windows[0][1]); gap < lag-time.Minute {
		t.Fatalf("window ends %v ago, want at least the %v vendor lag", gap, lag)
	}
}

// 配了回流却漏了域名或没开 cdn，必须在启动校验时就报出来，
// 否则那个任务每轮只会安静地报一次错。
func TestEdgeLogScheduleConfigIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		tune func(*config.Config)
		want string
	}{
		{
			"missing_domain",
			func(c *config.Config) {
				c.CDN = config.CDNConfig{Enable: true, AccessKey: "ak", SecretKey: "sk"}
				c.Maintenance = config.MaintenanceConfig{Enable: true, EdgeLogEveryMinutes: 60}
			},
			"edge_log_domain",
		},
		{
			"cdn_disabled",
			func(c *config.Config) {
				c.Maintenance = config.MaintenanceConfig{
					Enable: true, EdgeLogEveryMinutes: 60, EdgeLogDomain: "cdn.test",
				}
			},
			"cdn.enable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t.TempDir())
			tc.tune(&cfg)
			cfg.Normalize()
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// 本地盘没有处理能力。静默返回一条原图地址是最坏的结果——
// 调用方会以为自己拿到了缩略图，直到某天发现首页在加载几 MB 的原图。
func TestProcessedURLFailsLoudlyWithoutCapability(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "photo.png", pngBytes())

	mgr := svc.Manager.(*manager.DefaultManager)
	_, err := mgr.ProcessedURL(context.Background(), result.Resource.ID, "image/resize,w_100", nil)
	if !errors.Is(err, provider.ErrProcessUnsupported) {
		t.Fatalf("err = %v, want ErrProcessUnsupported", err)
	}
}

// 不带处理参数时退化成普通访问地址，调用方不必为「要不要处理」分两条路径。
func TestProcessedURLWithoutProcessFallsBack(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "photo.png", pngBytes())

	mgr := svc.Manager.(*manager.DefaultManager)
	got, err := mgr.ProcessedURL(context.Background(), result.Resource.ID, "", nil)
	if err != nil {
		t.Fatalf("processed url: %v", err)
	}
	want, err := svc.Manager.GetAccessURL(context.Background(), result.Resource.ID, nil)
	if err != nil {
		t.Fatalf("access url: %v", err)
	}
	if got != want {
		t.Fatalf("fallback url = %q, want %q", got, want)
	}
}

// CDN 鉴权模式下处理参数必须参与摘要：不同参数是不同的缓存键，
// 而且拼在签好的地址后面只会让整条地址验签失败。
func TestProcessedURLUnderCDNAuthIsSigned(t *testing.T) {
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
		c.Access.URLMode = config.URLModeCDNAuthA
		c.Access.CDNAuthKey = "cdn-secret"
		c.Access.CDNDomain = "edge.example.com"
	})
	result := upload(t, svc, "photo.png", pngBytes())

	mgr := svc.Manager.(*manager.DefaultManager)
	thumb, err := mgr.ProcessedURL(context.Background(), result.Resource.ID, "image/resize,w_100", nil)
	if err != nil {
		t.Fatalf("processed url: %v", err)
	}
	full, err := mgr.ProcessedURL(context.Background(), result.Resource.ID, "image/resize,w_800", nil)
	if err != nil {
		t.Fatalf("processed url: %v", err)
	}
	if !strings.Contains(thumb, "x-oss-process") {
		t.Fatalf("processed url lost its process parameter: %s", thumb)
	}
	if thumb == full {
		t.Fatal("different process parameters must yield different urls")
	}
	// 两条地址的鉴权摘要也必须不同，否则说明处理参数没进摘要。
	if authKeyOf(t, thumb) == authKeyOf(t, full) {
		t.Fatal("process parameter did not participate in the cdn auth digest")
	}
}

func authKeyOf(t *testing.T, raw string) string {
	t.Helper()
	idx := strings.Index(raw, "auth_key=")
	if idx < 0 {
		t.Fatalf("url %q has no auth_key", raw)
	}
	return raw[idx:]
}

type fakeRefresher struct {
	mu        sync.Mutex
	refreshed []string
}

func (f *fakeRefresher) Refresh(_ context.Context, urls []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, urls...)
	return nil
}

func (f *fakeRefresher) Preload(context.Context, []string) error { return nil }

func TestRefreshCDNRequiresImplementation(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "banner.png", pngBytes())

	mgr := svc.Manager.(*manager.DefaultManager)
	err := mgr.RefreshCDN(context.Background(), []string{result.Resource.ID}, nil)
	if !errors.Is(err, manager.ErrCDNRefresherMissing) {
		t.Fatalf("err = %v, want ErrCDNRefresherMissing", err)
	}
}

func TestRefreshCDNPassesPublicURLs(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "banner.png", pngBytes())

	refresher := &fakeRefresher{}
	mgr := svc.Manager.(*manager.DefaultManager)
	mgr.SetCDNRefresher(refresher)
	if err := mgr.RefreshCDN(context.Background(), []string{result.Resource.ID}, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	refresher.mu.Lock()
	defer refresher.mu.Unlock()
	if len(refresher.refreshed) != 1 {
		t.Fatalf("refreshed %d urls, want 1", len(refresher.refreshed))
	}
	if !strings.Contains(refresher.refreshed[0], result.Resource.ObjectKey) {
		t.Errorf("refreshed %q, want the object's public url", refresher.refreshed[0])
	}
}

// 刷新同样受租户边界约束：拿着别人的 ID 刷不到任何东西。
func TestRefreshCDNIsTenantScoped(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadForTenant(t, svc, "crm", "acct-a", "u-1")

	mgr := svc.Manager.(*manager.DefaultManager)
	mgr.SetCDNRefresher(&fakeRefresher{})
	err := mgr.RefreshCDN(context.Background(), []string{resource.ID},
		&model.AccessRequest{Namespace: "crm", AccountID: "acct-b"})
	if err == nil {
		t.Fatal("cross-tenant cdn refresh should be rejected")
	}
}
