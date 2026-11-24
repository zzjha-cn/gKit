package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/cdn"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// ErrEdgeLogUnsupported 没有注入能回捞边缘日志的实现。
var ErrEdgeLogUnsupported = errors.New("eresource: no cdn log fetcher configured")

// EdgeLogResult 一次边缘日志回流的结果。
type EdgeLogResult struct {
	Files     int   // 处理的日志文件数
	Lines     int   // 解析成功的行数
	Matched   int   // 能映射回本模块资源的行数
	Bytes     int64 // 边缘回给客户端的总字节数
	CacheHits int   // 命中边缘缓存的行数
}

// SetCDNLogFetcher 注入边缘日志回捞实现。约定在启动期调用。
func (m *DefaultManager) SetCDNLogFetcher(fetcher cdn.CDNLogFetcher) {
	if fetcher == nil {
		m.cdnLogs.Store((*cdn.CDNLogFetcher)(nil))
		return
	}
	m.cdnLogs.Store(&fetcher)
}

func (m *DefaultManager) cdnLogFetcher() cdn.CDNLogFetcher {
	stored, _ := m.cdnLogs.Load().(*cdn.CDNLogFetcher)
	if stored == nil {
		return nil
	}
	return *stored
}

// IngestEdgeLogs 回捞一段时间的 CDN 边缘日志，把下载量补回本模块的指标与审计。
//
// 这条链路补的是 §2.5 列出的那个窟窿：切到 CDN 鉴权之后请求**根本不到
// 业务服务**，下载量、命中率、谁下载了什么，模块这一侧全部失明。
// 没有它，「切 CDN 到底省了多少源站流量」这个问题连个能回答它的数字都没有。
//
// 回流的数据与实时埋点的区别要清楚：它是**延迟的**（厂商的日志通常
// 滞后数十分钟到数小时）、**可能重复的**（同一段时间被捞两次就会重复计数）。
// 因此调用方必须自己记住"上次捞到哪了"——模块不替它记，
// 那需要一张状态表，而这件事调用方往往已经有地方放了。
//
// route 标成 routeEdge，与实时埋点的 proxy / signed 区分开：
// 两者的采集方式与可信度不同，混在同一个维度里会让成本分析得出错的结论。
func (m *DefaultManager) IngestEdgeLogs(ctx context.Context, domain string, start, end time.Time) (*EdgeLogResult, error) {
	fetcher := m.cdnLogFetcher()
	if fetcher == nil {
		return nil, ErrEdgeLogUnsupported
	}
	files, err := fetcher.ListEdgeLogs(ctx, domain, start, end)
	if err != nil {
		return nil, err
	}
	result := &EdgeLogResult{}
	var errs []error
	for _, file := range files {
		if err = m.ingestOneEdgeLog(ctx, fetcher, file, result); err != nil {
			// 单个文件处理失败不中断整批：回流是补数，
			// 少补一个文件的代价远小于整次任务失败。
			errs = append(errs, fmt.Errorf("ingest %s: %w", file.Name, err))
			continue
		}
		result.Files++
	}
	return result, errors.Join(errs...)
}

func (m *DefaultManager) ingestOneEdgeLog(ctx context.Context, fetcher cdn.CDNLogFetcher,
	file cdn.EdgeLogFile, result *EdgeLogResult) error {
	body, err := fetcher.OpenEdgeLog(ctx, file)
	if err != nil {
		return err
	}
	defer body.Close()

	sink := m.metricsSink()
	return cdn.ScanEdgeAccessLog(body, func(entry *cdn.EdgeAccessLog) error {
		result.Lines++
		// 只统计真正回了内容的请求：404 / 403 不消耗出口带宽，
		// 把它们算进下载量会让成本分析虚高。
		if entry.StatusCode >= 400 {
			return nil
		}
		result.Bytes += entry.Bytes
		if entry.Hit {
			result.CacheHits++
		}
		objectKey := m.objectKeyFromEdgePath(entry.Path)
		if objectKey == "" {
			return nil
		}
		result.Matched++
		labels := map[string]string{"route": routeEdge}
		sink.Count(MetricDownloadTotal, 1, labels)
		sink.Count(MetricDownloadBytes, entry.Bytes, labels)
		if sampleObjectKey(objectKey, m.config.Manager.ObjectReadSampleRate) {
			sink.Count(MetricObjectRead, 1, map[string]string{"object_key": objectKey})
		}
		// 审计同样补上：合规要的是"谁在什么时候下载了什么"，
		// 而走 CDN 的那部分流量在实时链路上一条记录都没有。
		_ = m.Audit(ctx, &AuditEvent{
			At:        entry.At,
			Action:    AuditActionDownload,
			ObjectKey: objectKey,
			ClientIP:  entry.ClientIP,
			Bytes:     entry.Bytes,
			Route:     routeEdge,
		})
		return nil
	})
}

// routeEdge 标记来自 CDN 边缘日志回流的下载。
//
// 与实时埋点的 proxy / signed 分开：回流数据是延迟的、可能重复的，
// 混进同一个维度会让成本分析得出错的结论。
const routeEdge = "edge"

// objectKeyFromEdgePath 把边缘日志里的请求路径还原成 object key。
//
// 返回空串表示这条日志不属于本模块（同一个 CDN 域名下完全可能
// 还挂着别的业务），调用方据此跳过。
func (m *DefaultManager) objectKeyFromEdgePath(path string) string {
	key := strings.TrimPrefix(path, "/")
	if key == "" {
		return ""
	}
	// 签名路由下的路径带着验签前缀，要剥掉才是真正的 object key。
	signedPrefix := strings.Trim(m.config.Provider.SignedBasePath, "/")
	if signedPrefix != "" && strings.HasPrefix(key, signedPrefix+"/") {
		key = strings.TrimPrefix(key, signedPrefix+"/")
	}
	// base_path 是 object key 的一部分，不剥。
	// 这里只做一次存在性确认——路径里可能混着别的业务的请求，
	// 认错了会把别人的流量算进本模块的账。
	count, err := m.store.Count(context.Background(), &model.Query{ObjectKey: key})
	if err != nil || count == 0 {
		return ""
	}
	return key
}
