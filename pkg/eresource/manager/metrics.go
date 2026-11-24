package manager

import (
	"hash/fnv"
	"strconv"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// Metrics 是模块向外吐指标的窄接口。
//
// 刻意不绑定任何监控系统（Prometheus / OpenTelemetry / 自研）：
// 这个模块是被嵌进各种服务里的，强行引入一个指标库会把它的依赖
// 传染给每一个调用方。实现由调用方提供，接什么由调用方决定。
//
// 实现必须是**非阻塞**的。埋点挂在上传与下载的关键路径上，
// 一次同步写入会把存储系统的抖动直接变成业务延迟。
// 需要落盘 / 发远端请打到自己的缓冲里去异步处理。
//
// 实现还必须是并发安全的：同一个实例会被多个请求同时调用。
type Metrics interface {
	// Count 累加一个计数器。
	Count(name string, value int64, labels map[string]string)
	// Observe 记录一次观测值（耗时、体积）。
	Observe(name string, value float64, labels map[string]string)
}

// 指标名。取值刻意保持稳定——它们会出现在调用方的面板与告警规则里，
// 改名等于让对方的看板静默变空。
const (
	MetricUploadTotal   = "eresource_upload_total"
	MetricUploadBytes   = "eresource_upload_bytes"
	MetricDownloadTotal = "eresource_download_total"
	MetricDownloadBytes = "eresource_download_bytes"
	MetricDedupHit      = "eresource_dedup_hit_total"
	// MetricObjectRead 按 object key 统计读取次数，用于估算读放大。
	//
	// 读放大是决定「值不值得上 CDN」的唯一依据（Roadmap.md §2）：
	// 接近 1 说明人人读自己的，CDN 没有意义；上到几十几百才值得改造。
	MetricObjectRead = "eresource_object_read_total"
)

// nopMetrics 是默认实现：什么都不做，也不分配。
// 没注入 Metrics 的部署不会因为埋点付出任何代价。
type nopMetrics struct{}

func (nopMetrics) Count(string, int64, map[string]string)     {}
func (nopMetrics) Observe(string, float64, map[string]string) {}

// SetMetrics 替换指标实现。约定在启动期调用。
func (m *DefaultManager) SetMetrics(metrics Metrics) {
	if metrics == nil {
		metrics = nopMetrics{}
	}
	m.metrics.Store(&metrics)
}

// metricsSink 读取当前生效的指标实现。与 rateLimiter 同一套 atomic.Value 处理。
func (m *DefaultManager) metricsSink() Metrics {
	stored, _ := m.metrics.Load().(*Metrics)
	if stored == nil {
		return nopMetrics{}
	}
	return *stored
}

// countUpload 记录一次上传。uploaded 为 false 表示命中去重或幂等键，没有写入字节。
func (m *DefaultManager) countUpload(resource *model.Resource, uploaded bool) {
	sink := m.metricsSink()
	labels := map[string]string{
		"namespace": resource.Namespace,
		"biz_type":  resource.BizType,
		"policy":    resource.AccessPolicy,
		"uploaded":  strconv.FormatBool(uploaded),
	}
	sink.Count(MetricUploadTotal, 1, labels)
	if uploaded {
		sink.Count(MetricUploadBytes, resource.Size, labels)
	}
}

// CountDownload 记录一次下载。
//
// 由 transport 层在内容真正写完之后调用——路由层才知道这次走的是
// 代理读流、签名直出还是公开直链，而这正是 B12 要的成本分布。
// route 取值建议：proxy / signed / public。
func (m *DefaultManager) CountDownload(namespace, policy, route, objectKey string, bytes int64) {
	sink := m.metricsSink()
	labels := map[string]string{
		"namespace": namespace,
		"policy":    policy,
		"route":     route,
	}
	sink.Count(MetricDownloadTotal, 1, labels)
	sink.Count(MetricDownloadBytes, bytes, labels)
	// 读放大按 object key 统计，但**必须采样**：object key 的基数等于
	// 对象总数，全量打点会给监控后端造出一个百万级 label 的时间序列，
	// 足以把 Prometheus 这类系统直接压垮（高基数标签是它最经典的事故原因）。
	//
	// 采样 1% 不影响结论：这个指标要回答的是「热点有多热」——
	// 读放大是接近 1 还是上到几十几百，1% 的样本足够区分，
	// 而它恰恰是决定「值不值得上 CDN」的唯一依据（Roadmap §2）。
	// 代价是冷门对象可能一次都采不到，但冷门对象本来也不影响这个判断。
	if sampleObjectKey(objectKey, m.config.Manager.ObjectReadSampleRate) {
		sink.Count(MetricObjectRead, 1, map[string]string{"object_key": objectKey})
	}
}

// sampleObjectKey 按 object key 做确定性采样。
//
// 用 key 的哈希而不是随机数：同一个对象要么一直被采样、要么一直不被采样。
// 随机采样下，一个被读了 1000 次的热点和 1000 个各被读一次的冷门对象
// 会采出同样的样本分布，而区分这两者正是这个指标的全部意义。
//
// rate 是百分之一的分母：100 表示 1%，1 表示全量，<=0 表示不采集。
func sampleObjectKey(objectKey string, rate int) bool {
	if rate <= 0 {
		return false
	}
	if rate == 1 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(objectKey))
	return int(h.Sum32()%uint32(rate)) == 0
}

// SampleObjectKeyForTest 暴露采样判定供测试使用。
//
// 采样逻辑本身不该导出——它是实现细节。但"1% 采样确实把基数压下来了"
// 这件事必须能被测到，否则改坏了也没人知道。
func SampleObjectKeyForTest(objectKey string, rate int) bool {
	return sampleObjectKey(objectKey, rate)
}
