package cdn

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/rs/xid"
)

// AliCDNRefresher 基于阿里云 CDN OpenAPI 的刷新 / 预热实现。
//
// 刻意**不引入厂商 SDK**，只用标准库拼 RPC 请求。
// 理由很实在：这个模块会被嵌进各种服务里，其中大多数只用本地盘，
// 为了一个它们永远不会调的接口背上一整个 SDK 的依赖树不划算。
// 签名算法本身只有二十行。
//
// 落地前请对着当时的阿里云 CDN 文档核一遍 Action 名与参数——
// 这里的实现对应 RefreshObjectCaches / PushObjectCache（API 版本 2018-05-10）。
type AliCDNRefresher struct {
	accessKey string
	secretKey string
	endpoint  string
	client    *http.Client
}

// 刷新与预热的批量上限。
//
// 阿里云对单次请求的 URL 条数有限制，超了整批都会被拒。
// 在这里切批而不是让调用方自己注意：调用方是对着「刷新这些资源」
// 这个语义来的，不该被迫了解厂商的分页规则。
const cdnBatchSize = 100

// NewAliCDNRefresher 构造 CDN 刷新客户端。
func NewAliCDNRefresher(accessKey, secretKey, endpoint string) (*AliCDNRefresher, error) {
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("eresource: cdn access_key and secret_key are required")
	}
	if endpoint == "" {
		endpoint = "https://cdn.aliyuncs.com/"
	}
	return &AliCDNRefresher{
		accessKey: accessKey,
		secretKey: secretKey,
		endpoint:  endpoint,
		// 独立的 client 而不是 http.DefaultClient：默认客户端没有超时，
		// 一次卡住的刷新会把调用方的运维脚本挂死。
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (r *AliCDNRefresher) Name() string { return "aliyun_cdn" }

// Refresh 让这些 URL 在边缘立即失效。
func (r *AliCDNRefresher) Refresh(ctx context.Context, urls []string) error {
	return r.batched(ctx, urls, "RefreshObjectCaches", "ObjectPath")
}

// Preload 把这些 URL 预热到边缘。
func (r *AliCDNRefresher) Preload(ctx context.Context, urls []string) error {
	return r.batched(ctx, urls, "PushObjectCache", "ObjectPath")
}

func (r *AliCDNRefresher) batched(ctx context.Context, urls []string, action, field string) error {
	for start := 0; start < len(urls); start += cdnBatchSize {
		end := start + cdnBatchSize
		if end > len(urls) {
			end = len(urls)
		}
		// 阿里云用换行分隔多条 URL。
		payload := strings.Join(urls[start:end], "\n")
		if err := r.call(ctx, action, map[string]string{field: payload}); err != nil {
			return err
		}
	}
	return nil
}

// call 发起一次签名过的 RPC 请求，丢弃响应体。
func (r *AliCDNRefresher) call(ctx context.Context, action string, params map[string]string) error {
	_, err := r.callJSON(ctx, action, params)
	return err
}

// callJSON 发起一次签名过的 RPC 请求并返回响应体。
func (r *AliCDNRefresher) callJSON(ctx context.Context, action string, params map[string]string) ([]byte, error) {
	query := map[string]string{
		"Action":           action,
		"Format":           "JSON",
		"Version":          "2018-05-10",
		"AccessKeyId":      r.accessKey,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		// Nonce 必须每次不同，否则阿里云会把重复请求判成重放并拒掉。
		"SignatureNonce": xid.New().String(),
		"Timestamp":      time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	for k, v := range params {
		query[k] = v
	}
	query["Signature"] = rpcSignature("POST", query, r.secretKey)

	form := url.Values{}
	for k, v := range query {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdn %s request failed: %w", action, err)
	}
	defer resp.Body.Close()
	// 日志列表的响应可能不小，上限给到 8MB。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		// 把厂商返回的 Code / Message 带出来：只报一个 400
		// 会让排查从「读错误信息」变成「翻文档猜」。
		var apiErr struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		}
		_ = json.Unmarshal(body, &apiErr)
		return nil, fmt.Errorf("cdn %s failed: http %d code=%s message=%s",
			action, resp.StatusCode, apiErr.Code, apiErr.Message)
	}
	return body, nil
}

// rpcSignature 按阿里云 RPC 风格的 V1 签名规则计算摘要。
//
//	StringToSign = HTTPMethod + "&" + percentEncode("/") + "&" + percentEncode(canonicalizedQuery)
//	Signature    = Base64(HMAC-SHA1(AccessKeySecret + "&", StringToSign))
//
// 注意两处容易写错的地方：参数要按**键名字典序**排序，
// 以及密钥末尾那个 "&" 不是笔误，是规范要求的。
func rpcSignature(method string, params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "Signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonical strings.Builder
	for i, k := range keys {
		if i > 0 {
			canonical.WriteString("&")
		}
		canonical.WriteString(percentEncode(k))
		canonical.WriteString("=")
		canonical.WriteString(percentEncode(params[k]))
	}
	stringToSign := method + "&" + percentEncode("/") + "&" + percentEncode(canonical.String())

	mac := hmac.New(sha1.New, []byte(secret+"&"))
	_, _ = mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// percentEncode 是 RFC 3986 的转义，与 url.QueryEscape 有三处差异。
//
// 这三处差异正是签名最常见的失败原因：QueryEscape 把空格编码成 "+"、
// 不编码 "*"、把 "~" 编码成 "%7E"，三条都与规范相反。
func percentEncode(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "+", "%20")
	escaped = strings.ReplaceAll(escaped, "*", "%2A")
	escaped = strings.ReplaceAll(escaped, "%7E", "~")
	return escaped
}

var _ CDNRefresher = (*AliCDNRefresher)(nil)
