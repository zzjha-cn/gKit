package manager

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// MaintenanceOptions 后台维护任务的调度参数。
//
// 这些任务此前都只是可调用的方法（PurgeExpired / ReconcileUsage /
// ReconcileOrphans），需要调用方自己接定时器。对绝大多数接入方来说
// 那是一段必然会被忘掉的代码——而忘掉的后果是软删的对象永远不清、
// 用量账面一路漂、孤儿对象持续计费，三件事都只会在账单上才显形。
//
// 所以提供一个**默认关闭、打开即用**的调度器。保持 opt-in 是刻意的：
// 后台任务会扫全表、会删对象，不该由一次库升级悄悄开始跑。
type MaintenanceOptions struct {
	// PurgeExpiredEvery 清理过期软删资源的间隔，<=0 表示不跑。
	// 依赖 access.purge_retention_days。
	PurgeExpiredEvery time.Duration
	// PurgeLimit 单批清理条数，<=0 取 100。
	PurgeLimit int64

	// ReconcileUsageEvery 用量对账间隔，<=0 表示不跑。
	// Roadmap §5 的建议是每天一次。
	ReconcileUsageEvery time.Duration
	// CorrectUsage 对账时是否直接修正账面。
	//
	// 默认 false——只报告。第一次在生产上跑应当先看一眼偏差有多大：
	// 偏差巨大往往意味着别处出了事故，这时候把账面改成"真实值"
	// 等于把现场也抹平。确认偏差是正常累积之后再打开。
	CorrectUsage bool

	// EdgeLogEvery CDN 边缘日志回流的间隔，<=0 表示不跑。
	//
	// 需要同时配 EdgeLogDomain。回流补的是「切了 CDN 之后下载量失明」
	// 这个窟窿（见 IngestEdgeLogs）。
	EdgeLogEvery time.Duration
	// EdgeLogDomain 要回捞日志的 CDN 域名。
	EdgeLogDomain string
	// EdgeLogLag 回捞时向前退的时间量。
	//
	// 厂商的日志有生成延迟（通常数十分钟），捞"刚刚过去的那一分钟"
	// 只会捞到空文件。<=0 取 DefaultEdgeLogLag。
	EdgeLogLag time.Duration
	// EdgeLogInitialLookback 首次回捞覆盖的时间跨度，<=0 取 DefaultEdgeLogLookback。
	//
	// 进度游标只在内存里：进程重启后会从 now-lag-lookback 重新开始，
	// 这段区间可能被重复统计。这是刻意的取舍——持久化游标需要一张
	// 状态表，而回流本来就是「补个大概」的成本分析数据，
	// 不是账务。需要精确不重复的，自己调 IngestEdgeLogs 并自己记进度。
	EdgeLogInitialLookback time.Duration

	// TieringEvery 存储分层策略的执行间隔，<=0 表示不跑。
	TieringEvery time.Duration
	// TieringPolicies 要执行的分层策略。
	TieringPolicies []TieringPolicy

	// ExpireIdempotencyEvery 幂等键过期清理的间隔，<=0 表示不跑。
	// 不跑的后果是那条唯一稀疏索引随上传量无限增长。
	ExpireIdempotencyEvery time.Duration
	// ExpireIdempotencyLimit 单批清理条数，<=0 取 100。
	ExpireIdempotencyLimit int64

	// ReconcileOrphansEvery 孤儿对象对账间隔，<=0 表示不跑。
	ReconcileOrphansEvery time.Duration
	// OrphanSafetyWindow 孤儿判定的安全窗口，必须 > 0 才会启用该任务。
	//
	// 没有默认值是刻意的：窗口要大于最长一次上传耗时，开了直传还要
	// 大于 direct_upload_expire，这些只有部署方知道。给一个默认值
	// 等于替它猜，而猜错的后果是删掉用户正在传的文件。
	OrphanSafetyWindow time.Duration
	// OrphanBatchLimit 单批扫描的对象数，<=0 由 provider 取默认值。
	OrphanBatchLimit int
	// OrphanDryRun 只报告不删除。
	OrphanDryRun bool
}

// Maintenance 后台维护任务的运行器。
type Maintenance struct {
	mgr   *DefaultManager
	opts  MaintenanceOptions
	stop  chan struct{}
	done  sync.WaitGroup
	once  sync.Once
	clock func() time.Time

	// edgeCursor 记录边缘日志已经回捞到哪个时刻（仅内存）。
	edgeMu     sync.Mutex
	edgeCursor time.Time
}

// 边缘日志回流的默认参数。
const (
	// DefaultEdgeLogLag 回捞时向前退的时间量，覆盖厂商的日志生成延迟。
	DefaultEdgeLogLag = 30 * time.Minute
	// DefaultEdgeLogLookback 首次回捞覆盖的跨度。
	DefaultEdgeLogLookback = 2 * time.Hour
)

// StartMaintenance 按配置启动后台维护任务，返回的 Maintenance 用于停止。
//
// 每个任务一个独立 goroutine：一个任务卡住（比如对账在扫一个超大租户）
// 不该把另外两个一起拖死。
func (m *DefaultManager) StartMaintenance(opts MaintenanceOptions) *Maintenance {
	maintenance := &Maintenance{
		mgr:   m,
		opts:  opts,
		stop:  make(chan struct{}),
		clock: time.Now,
	}
	if opts.PurgeExpiredEvery > 0 {
		maintenance.run(opts.PurgeExpiredEvery, "purge_expired", maintenance.purgeExpired)
	}
	if opts.ReconcileUsageEvery > 0 {
		maintenance.run(opts.ReconcileUsageEvery, "reconcile_usage", maintenance.reconcileUsage)
	}
	if opts.EdgeLogEvery > 0 && opts.EdgeLogDomain != "" {
		lookback := opts.EdgeLogInitialLookback
		if lookback <= 0 {
			lookback = DefaultEdgeLogLookback
		}
		maintenance.edgeCursor = maintenance.clock().Add(-maintenance.edgeLag() - lookback)
		maintenance.run(opts.EdgeLogEvery, "edge_logs", maintenance.ingestEdgeLogs)
	}
	if opts.TieringEvery > 0 && len(opts.TieringPolicies) > 0 {
		maintenance.run(opts.TieringEvery, "tiering", maintenance.applyTiering)
	}
	if opts.ExpireIdempotencyEvery > 0 {
		maintenance.run(opts.ExpireIdempotencyEvery, "expire_idempotency", maintenance.expireIdempotency)
	}
	if opts.ReconcileOrphansEvery > 0 && opts.OrphanSafetyWindow > 0 {
		maintenance.run(opts.ReconcileOrphansEvery, "reconcile_orphans", maintenance.reconcileOrphans)
	}
	return maintenance
}

// run 起一个按固定间隔触发的任务。
//
// 首次触发等一个完整间隔，不在启动瞬间就跑：进程刚起来时
// 连接池、索引、依赖都还没热，这时候压一个全表扫描进去，
// 最容易把一次正常的滚动发布变成一次抖动。
func (t *Maintenance) run(every time.Duration, name string, fn func(context.Context)) {
	t.done.Add(1)
	go func() {
		defer t.done.Done()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-ticker.C:
				// 每轮独立 ctx：一轮跑飞了不影响下一轮。
				// 超时取间隔本身——一轮还没跑完下一轮就该开始了的话，
				// 说明这个间隔配得太短，继续堆积只会越陷越深。
				ctx, cancel := context.WithTimeout(context.Background(), every)
				fn(ctx)
				cancel()
				_ = name
			}
		}
	}()
}

func (t *Maintenance) purgeExpired(ctx context.Context) {
	purged, err := t.mgr.PurgeExpired(ctx, t.opts.PurgeLimit)
	if err != nil {
		slog.Error("eresource: scheduled purge failed", "err", err, "purged", purged)
		return
	}
	if purged > 0 {
		slog.Info("eresource: scheduled purge completed", "purged", purged)
	}
}

func (t *Maintenance) reconcileUsage(ctx context.Context) {
	drifts, err := t.mgr.ReconcileAllUsage(ctx, t.opts.CorrectUsage)
	if err != nil {
		slog.Error("eresource: scheduled usage reconcile failed", "err", err)
		return
	}
	for _, drift := range drifts {
		// 漂移一律打日志，哪怕已经修正了：一个持续漂移的租户
		// 说明别处有 bug，而修正会把症状抹掉，只留下日志这一条线索。
		slog.Warn("eresource: tenant usage drift detected", "namespace", drift.Namespace,
			"account_id", drift.AccountID,
			"bytes_diff", drift.BytesDiff,
			"files_diff", drift.FilesDiff,
			"corrected", drift.Corrected)
	}
}

func (t *Maintenance) edgeLag() time.Duration {
	if t.opts.EdgeLogLag > 0 {
		return t.opts.EdgeLogLag
	}
	return DefaultEdgeLogLag
}

func (t *Maintenance) ingestEdgeLogs(ctx context.Context) {
	// 窗口右边界要向前退一个 lag：厂商的日志有生成延迟，
	// 捞"刚刚过去的那一分钟"只会捞到空文件，而游标却已经推过去了。
	end := t.clock().Add(-t.edgeLag())

	t.edgeMu.Lock()
	start := t.edgeCursor
	t.edgeMu.Unlock()

	if !start.Before(end) {
		return
	}
	result, err := t.mgr.IngestEdgeLogs(ctx, t.opts.EdgeLogDomain, start, end)
	if err != nil {
		// 失败时**不推进游标**：下一轮会把这段重新捞一遍。
		// 宁可重复统计也不要留一段永远补不上的空洞——
		// 前者让曲线偏高一点，后者会让"切 CDN 省了多少"直接算错。
		slog.Error("eresource: scheduled edge log ingest failed", "err", err, "domain", t.opts.EdgeLogDomain, "start", start, "end", end)
		return
	}
	t.edgeMu.Lock()
	t.edgeCursor = end
	t.edgeMu.Unlock()

	if result.Matched > 0 {
		slog.Info("eresource: edge logs ingested", "files", result.Files, "lines", result.Lines,
			"matched", result.Matched, "bytes", result.Bytes,
			"cache_hits", result.CacheHits)
	}
}

// EdgeCursor 返回边缘日志已经回捞到的时刻，供监控与测试观察进度。
func (t *Maintenance) EdgeCursor() time.Time {
	t.edgeMu.Lock()
	defer t.edgeMu.Unlock()
	return t.edgeCursor
}

func (t *Maintenance) applyTiering(ctx context.Context) {
	results, err := t.mgr.ApplyTieringPolicies(ctx, t.opts.TieringPolicies)
	if err != nil {
		slog.Error("eresource: scheduled tiering failed", "err", err)
	}
	for _, result := range results {
		slog.Info("eresource: tiering policy applied", "policy", result.Policy,
			"storage_class", result.StorageClass,
			"moved", result.Moved)
	}
}

func (t *Maintenance) expireIdempotency(ctx context.Context) {
	cleared, err := t.mgr.ExpireIdempotencyKeys(ctx, t.opts.ExpireIdempotencyLimit)
	if err != nil {
		slog.Error("eresource: scheduled idempotency expiry failed", "err", err, "cleared", cleared)
		return
	}
	if cleared > 0 {
		slog.Info("eresource: expired idempotency keys", "cleared", cleared)
	}
}

func (t *Maintenance) reconcileOrphans(ctx context.Context) {
	result, err := t.mgr.ReconcileOrphans(ctx, &ReconcileOptions{
		Before: t.clock().Add(-t.opts.OrphanSafetyWindow),
		Limit:  t.opts.OrphanBatchLimit,
		DryRun: t.opts.OrphanDryRun,
	})
	if err != nil {
		slog.Error("eresource: scheduled orphan reconcile failed", "err", err)
		return
	}
	if result.Orphans > 0 {
		slog.Warn("eresource: orphan objects found", "scanned", result.Scanned,
			"orphans", result.Orphans,
			"deleted", result.Deleted,
			"dry_run", t.opts.OrphanDryRun)
	}
}

// Stop 停止全部任务并等待它们退出。重复调用是安全的。
func (t *Maintenance) Stop() {
	if t == nil {
		return
	}
	t.once.Do(func() { close(t.stop) })
	t.done.Wait()
}
