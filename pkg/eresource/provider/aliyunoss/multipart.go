package aliyunoss

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

func (p *AliOSSProvider) InitMultipart(ctx context.Context, req *model.UploadRequest) (*model.MultipartInit, error) {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return nil, err
	}
	objectKey, err := p.objectKey(req)
	if err != nil {
		return nil, err
	}
	contentType := req.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	options := []oss.Option{
		oss.ContentType(contentType),
		oss.ObjectACL(objectACL(req.AccessPolicy)),
		oss.WithContext(ctx),
	}
	// 加密同样要在 Init 时声明：分片上传的加密属性在初始化阶段定下，
	// 之后每一片都继承它，Complete 时再补是补不上的。
	options = append(options, p.encryptionOptions()...)

	result, err := bucket.InitiateMultipartUpload(objectKey, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to init multipart upload: %w", err)
	}
	return &model.MultipartInit{
		ObjectKey: result.Key,
		UploadID:  result.UploadID,
		Provider:  p.Name(),
		Bucket:    p.bucketName,
		StorePath: fmt.Sprintf("%s.%s/%s", p.bucketName, p.endpoint, objectKey),
	}, nil
}

func (p *AliOSSProvider) UploadPart(ctx context.Context, objectKey, uploadID string,
	partNumber int, body io.Reader, size int64) (*model.MultipartPart, error) {
	if err := provider.ValidatePartNumber(partNumber); err != nil {
		return nil, err
	}
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return nil, err
	}
	imur := oss.InitiateMultipartUploadResult{Bucket: p.bucketName, Key: objectKey, UploadID: uploadID}
	part, err := bucket.UploadPart(imur, body, size, partNumber, oss.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("failed to upload part %d: %w", partNumber, err)
	}
	return &model.MultipartPart{PartNumber: part.PartNumber, ETag: part.ETag, Size: size}, nil
}

func (p *AliOSSProvider) CompleteMultipart(ctx context.Context, objectKey, uploadID string,
	parts []model.MultipartPart) (*model.ProviderResult, error) {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return nil, err
	}
	imur := oss.InitiateMultipartUploadResult{Bucket: p.bucketName, Key: objectKey, UploadID: uploadID}
	ossParts := make([]oss.UploadPart, 0, len(parts))
	var total int64
	for _, part := range parts {
		ossParts = append(ossParts, oss.UploadPart{
			PartNumber: part.PartNumber,
			// OSS 返回的 ETag 带引号，回传时必须原样带上，
			// 自作主张去掉会让合并请求报 InvalidPart。
			ETag: part.ETag,
		})
		total += part.Size
	}
	sort.Slice(ossParts, func(i, j int) bool { return ossParts[i].PartNumber < ossParts[j].PartNumber })
	if _, err = bucket.CompleteMultipartUpload(imur, ossParts, oss.WithContext(ctx)); err != nil {
		return nil, fmt.Errorf("failed to complete multipart upload: %w", err)
	}
	// 以 OSS 侧的真实大小为准：客户端声明的分片大小不可信。
	if stat, statErr := p.Stat(ctx, objectKey); statErr == nil && stat != nil && stat.Exists {
		total = stat.Size
	}
	return &model.ProviderResult{
		Provider:  p.Name(),
		Bucket:    p.bucketName,
		ObjectKey: objectKey,
		StorePath: fmt.Sprintf("%s.%s/%s", p.bucketName, p.endpoint, objectKey),
		Size:      total,
	}, nil
}

func (p *AliOSSProvider) AbortMultipart(ctx context.Context, objectKey, uploadID string) error {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return err
	}
	imur := oss.InitiateMultipartUploadResult{Bucket: p.bucketName, Key: objectKey, UploadID: uploadID}
	return bucket.AbortMultipartUpload(imur, oss.WithContext(ctx))
}
