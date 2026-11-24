package aliyunoss

import (
	"context"
	"fmt"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// SetObjectACL 按访问策略重设对象 ACL。
//
// 映射规则与上传时完全一致（见 objectACL）：认不出的策略一律落到 private。
func (p *AliOSSProvider) SetObjectACL(ctx context.Context, objectKey, accessPolicy string) error {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return fmt.Errorf("failed to get bucket: %w", err)
	}
	if err = bucket.SetObjectACL(objectKey, objectACL(accessPolicy), oss.WithContext(ctx)); err != nil {
		return fmt.Errorf("failed to set object acl: %w", err)
	}
	return nil
}
