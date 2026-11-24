package aliyunoss

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// OpenRange 按字节区间读取 OSS 对象。
//
// 走 OSS 的 Range 请求而不是「拉全量再丢弃前面的字节」：后者的流量账单
// 与不支持 Range 完全一样，等于没做。
func (p *AliOSSProvider) OpenRange(ctx context.Context, objectKey string, offset, length int64) (io.ReadCloser, *model.ObjectStat, error) {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get bucket: %w", err)
	}
	options := []oss.Option{oss.WithContext(ctx)}
	if offset > 0 || length > 0 {
		if length > 0 {
			options = append(options, oss.Range(offset, offset+length-1))
		} else {
			// 没有上界：用 "bytes=offset-" 这种开区间写法。
			options = append(options, oss.NormalizedRange(strconv.FormatInt(offset, 10)+"-"))
		}
	}
	result, err := bucket.DoGetObject(&oss.GetObjectRequest{ObjectKey: objectKey}, options)
	if err != nil {
		if isNotFoundErr(err) {
			return nil, nil, fmt.Errorf("%w: object %q", fs.ErrNotExist, objectKey)
		}
		return nil, nil, fmt.Errorf("failed to get object range from aliyun oss: %w", err)
	}
	stat := objectStatFromHeader(result.Response.Headers)
	// 区间请求的 Content-Length 是本次区间的长度，不是对象大小。
	// 完整大小只能从 Content-Range 的分母里拿。
	if total, ok := totalSizeFromContentRange(result.Response.Headers.Get("Content-Range")); ok {
		stat.Size = total
	}
	return result.Response.Body, stat, nil
}

// totalSizeFromContentRange 解析 "bytes 0-499/1234" 里的对象总大小。
// 分母是 "*"（总长未知）或格式不符时返回 false，调用方保留原有的 Size。
func totalSizeFromContentRange(value string) (int64, bool) {
	_, after, found := strings.Cut(value, "/")
	if !found {
		return 0, false
	}
	total, err := strconv.ParseInt(strings.TrimSpace(after), 10, 64)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}
