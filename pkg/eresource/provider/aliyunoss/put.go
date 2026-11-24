package aliyunoss

import (
	"bytes"
	"context"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// PutObject 按指定 key 原样写入 OSS。
func (p *AliOSSProvider) PutObject(ctx context.Context, objectKey string, content []byte, contentType, accessPolicy string) error {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	options := []oss.Option{
		oss.ContentType(contentType),
		// ACL 跟着源资源的访问策略走：副本桶一旦哪天要顶上来直接服务，
		// 权限必须和主存储一致，而不是继承副本桶的默认值。
		oss.ObjectACL(objectACL(accessPolicy)),
		oss.WithContext(ctx),
	}
	options = append(options, p.encryptionOptions()...)
	return bucket.PutObject(objectKey, bytes.NewReader(content), options...)
}
