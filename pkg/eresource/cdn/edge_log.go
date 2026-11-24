package cdn

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// EdgeAccessLog 一条 CDN 边缘访问日志。
//
// 只保留模块关心的字段。边缘日志的原始行还带着很多东西
// （refer、UA、回源耗时……），全量建模等于把厂商的日志格式
// 变成本模块的公开契约，而那个格式是会变的。
type EdgeAccessLog struct {
	At         time.Time
	ClientIP   string
	Method     string
	Path       string // 请求路径，可由此反查 object key
	StatusCode int
	Bytes      int64 // 边缘回给客户端的字节数
	Hit        bool  // 是否命中边缘缓存
}

// EdgeLogFile 一个可下载的日志文件。
type EdgeLogFile struct {
	Name      string
	URL       string
	StartTime time.Time
	EndTime   time.Time
	Size      int64
}

// CDNLogFetcher 由能够回捞 CDN 边缘日志的实现提供。
//
// 为什么这件事必须有：切到 CDN 鉴权之后请求**根本不到业务服务**，
// 下载量、命中率、谁下载了什么，模块这一侧全部失明（README 3.13.1
// 那张"会失去什么"的表就是在说这件事）。边缘日志回流是把这部分
// 可观测性拿回来的唯一途径——否则"省了多少源站流量"这个问题
// 连个能回答它的数字都没有。
type CDNLogFetcher interface {
	// ListEdgeLogs 列出某个域名在时间区间内的日志文件。
	ListEdgeLogs(ctx context.Context, domain string, start, end time.Time) ([]EdgeLogFile, error)
	// OpenEdgeLog 打开一个日志文件的内容流。
	OpenEdgeLog(ctx context.Context, file EdgeLogFile) (io.ReadCloser, error)
}

// ListEdgeLogs 调 DescribeCdnDomainLogs 列出日志文件。
func (r *AliCDNRefresher) ListEdgeLogs(ctx context.Context, domain string, start, end time.Time) ([]EdgeLogFile, error) {
	body, err := r.callJSON(ctx, "DescribeCdnDomainLogs", map[string]string{
		"DomainName": domain,
		"StartTime":  start.UTC().Format("2006-01-02T15:04Z"),
		"EndTime":    end.UTC().Format("2006-01-02T15:04Z"),
		"PageSize":   "300",
	})
	if err != nil {
		return nil, err
	}
	// 阿里云的返回是三层嵌套，直接按需解构，不引入一整套响应模型：
	// 那套模型只会在厂商改字段时变成一份需要同步维护的死代码。
	var payload struct {
		DomainLogDetails struct {
			DomainLogDetail []struct {
				LogInfos struct {
					LogInfoDetail []struct {
						LogName   string `json:"LogName"`
						LogPath   string `json:"LogPath"`
						LogSize   int64  `json:"LogSize"`
						StartTime string `json:"StartTime"`
						EndTime   string `json:"EndTime"`
					} `json:"LogInfoDetail"`
				} `json:"LogInfos"`
			} `json:"DomainLogDetail"`
		} `json:"DomainLogDetails"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode cdn log list: %w", err)
	}
	var files []EdgeLogFile
	for _, detail := range payload.DomainLogDetails.DomainLogDetail {
		for _, info := range detail.LogInfos.LogInfoDetail {
			logURL := info.LogPath
			// LogPath 回来时经常不带协议，直接拿去 http.Get 会失败。
			if !strings.HasPrefix(logURL, "http://") && !strings.HasPrefix(logURL, "https://") {
				logURL = "https://" + strings.TrimPrefix(logURL, "//")
			}
			files = append(files, EdgeLogFile{
				Name:      info.LogName,
				URL:       logURL,
				Size:      info.LogSize,
				StartTime: parseCDNTime(info.StartTime),
				EndTime:   parseCDNTime(info.EndTime),
			})
		}
	}
	return files, nil
}

// OpenEdgeLog 下载一个日志文件。
//
// 日志地址是带签名的临时链接，不需要再附加凭证——
// 这也意味着它会过期，拿到之后应当尽快下载。
func (r *AliCDNRefresher) OpenEdgeLog(ctx context.Context, file EdgeLogFile) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download cdn log %s: http %d", file.Name, resp.StatusCode)
	}
	return resp.Body, nil
}

func parseCDNTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z", "2006-01-02T15:04:05Z"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// ParseEdgeAccessLog 解析阿里云 CDN 的行式访问日志。
//
// 格式（空格分隔）：
//
//	[time] client_ip proxy_ip response_time "referer" "method url protocol" status size "" "ua" hit
//
// 解析刻意**宽容**：遇到不认识的行跳过而不是报错。日志格式是厂商那边的
// 实现细节，它变一次就让整条回流链路报错停摆，代价远大于丢几行统计。
func ParseEdgeAccessLog(line string) (*EdgeAccessLog, bool) {
	fields := splitLogLine(line)
	// 字段数按最短可用形态卡：时间 + IP + ... + 请求 + 状态 + 大小。
	if len(fields) < 8 {
		return nil, false
	}
	entry := &EdgeAccessLog{
		At:       parseBracketTime(fields[0]),
		ClientIP: fields[1],
	}
	// 请求行形如 "GET http://host/path HTTP/1.1"
	for _, field := range fields {
		if parts := strings.Fields(field); len(parts) == 3 && strings.HasPrefix(parts[2], "HTTP/") {
			entry.Method = parts[0]
			if parsed, err := url.Parse(parts[1]); err == nil {
				entry.Path = parsed.Path
			} else {
				entry.Path = parts[1]
			}
			break
		}
	}
	if entry.Path == "" {
		return nil, false
	}
	// 状态码与体积紧跟在请求行之后，用"第一个合法状态码"定位，
	// 比按固定下标取更抗格式微调。
	for i := 0; i+1 < len(fields); i++ {
		code, err := strconv.Atoi(strings.TrimSpace(fields[i]))
		if err != nil || code < 100 || code > 599 {
			continue
		}
		size, sizeErr := strconv.ParseInt(strings.TrimSpace(fields[i+1]), 10, 64)
		if sizeErr != nil {
			continue
		}
		entry.StatusCode = code
		entry.Bytes = size
		break
	}
	if entry.StatusCode == 0 {
		return nil, false
	}
	entry.Hit = strings.Contains(strings.ToUpper(line), "HIT")
	return entry, true
}

// splitLogLine 按空格切分，但把成对引号里的内容当作一个字段。
func splitLogLine(line string) []string {
	var fields []string
	var current strings.Builder
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			if !inQuote {
				fields = append(fields, current.String())
				current.Reset()
			}
		case r == ' ' && !inQuote:
			if current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields
}

func parseBracketTime(value string) time.Time {
	trimmed := strings.Trim(value, "[]")
	for _, layout := range []string{"02/Jan/2006:15:04:05 -0700", time.RFC3339} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// ScanEdgeAccessLog 逐行解析一个日志流。
func ScanEdgeAccessLog(reader io.Reader, fn func(*EdgeAccessLog) error) error {
	scanner := bufio.NewScanner(reader)
	// CDN 日志单行可能很长（URL + UA），默认 64KB 的上限会直接截断。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		entry, ok := ParseEdgeAccessLog(scanner.Text())
		if !ok {
			continue
		}
		if err := fn(entry); err != nil {
			return err
		}
	}
	return scanner.Err()
}

var _ CDNLogFetcher = (*AliCDNRefresher)(nil)
