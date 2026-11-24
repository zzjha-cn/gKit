package aliyunoss

import (
	"context"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/internal/object"
)

// ListObjects 走 OSS 的分页列举。
func (p *AliOSSProvider) ListObjects(ctx context.Context, prefix, marker string, limit int) ([]model.ObjectEntry, string, error) {
	limit = object.NormalizeListLimit(limit)
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return nil, "", err
	}
	// base_path 是 object key 的一部分，列举时要带上，
	// 否则对账会扫到同一个 bucket 里别的业务的对象。
	fullPrefix := prefix
	if p.basePath != "" && !strings.HasPrefix(prefix, p.basePath) {
		fullPrefix = strings.TrimSuffix(p.basePath, "/") + "/" + strings.TrimPrefix(prefix, "/")
		fullPrefix = strings.TrimSuffix(fullPrefix, "/")
	}
	result, err := bucket.ListObjects(
		oss.Prefix(fullPrefix),
		oss.Marker(marker),
		oss.MaxKeys(limit),
		oss.WithContext(ctx),
	)
	if err != nil {
		return nil, "", err
	}
	entries := make([]model.ObjectEntry, 0, len(result.Objects))
	for _, obj := range result.Objects {
		entries = append(entries, model.ObjectEntry{
			Key:       obj.Key,
			Size:      obj.Size,
			UpdatedAt: obj.LastModified,
		})
	}
	if result.IsTruncated {
		return entries, result.NextMarker, nil
	}
	return entries, "", nil
}
