package aliyunoss

import (
	"context"
	"fmt"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// SetStorageClass 通过「原地 CopyObject + 指定存储类型」完成转换。
//
// OSS 没有单独的「改存储类型」接口，官方做法就是覆盖拷贝自身。
// 注意它会刷新对象的 LastModified——孤儿对账的安全窗口因此会被重置，
// 这无害（对象本来就有记录指向），但看监控时要知道为什么时间变了。
func (p *AliOSSProvider) SetStorageClass(ctx context.Context, objectKey, storageClass string) error {
	switch storageClass {
	case provider.StorageClassStandard, provider.StorageClassIA, provider.StorageClassArchive:
	default:
		return fmt.Errorf("%w: unknown storage class %q", provider.ErrStorageClassUnsupported, storageClass)
	}
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return err
	}
	_, err = bucket.CopyObject(objectKey, objectKey,
		oss.ObjectStorageClass(oss.StorageClassType(storageClass)),
		// 元数据必须显式声明替换，否则 OSS 会拒绝「源与目标相同」的拷贝。
		oss.MetadataDirective(oss.MetaReplace),
		oss.WithContext(ctx),
	)
	return err
}
