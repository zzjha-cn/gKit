package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// MultipartUploader 由支持分片上传的 Provider 实现。
//
// 分片上传解决的是「单次 POST 传不完」——网络中断要能从断点接着传，
// 而不是从头再来一遍 2GB。它与前端直传正交：直传省的是服务端中转的带宽，
// 分片省的是失败重传的代价。
//
// 会话状态（哪些分片已经传完）**不由本模块保存**：
// OSS 侧天然记着 uploadID 下已上传的分片，本地实现则落在临时目录里。
// 服务端不维护中间表，客户端只需要原样带回票据。
type MultipartUploader interface {
	// InitMultipart 开启一次分片上传，返回 object key 与 uploadID。
	InitMultipart(ctx context.Context, req *model.UploadRequest) (*model.MultipartInit, error)
	// UploadPart 上传一个分片。partNumber 从 1 起算。
	UploadPart(ctx context.Context, objectKey, uploadID string, partNumber int, body io.Reader, size int64) (*model.MultipartPart, error)
	// CompleteMultipart 合并分片，返回与普通上传一致的结果。
	CompleteMultipart(ctx context.Context, objectKey, uploadID string, parts []model.MultipartPart) (*model.ProviderResult, error)
	// AbortMultipart 放弃本次上传并清理已传分片。
	AbortMultipart(ctx context.Context, objectKey, uploadID string) error
}

var (
	// ErrMultipartUnsupported 当前 provider 不支持分片上传。
	ErrMultipartUnsupported = errors.New("eresource: provider does not support multipart upload")
	// ErrPartNumberInvalid 分片序号非法。
	ErrPartNumberInvalid = errors.New("eresource: part number is out of range")
	// ErrUploadSessionInvalid 分片会话不存在或已被清理。
	ErrUploadSessionInvalid = errors.New("eresource: multipart upload session is invalid")
)

// ValidatePartNumber 校验分片序号是否在允许范围内。
func ValidatePartNumber(partNumber int) error {
	if partNumber < 1 || partNumber > model.MaxPartNumber {
		return fmt.Errorf("%w: %d", ErrPartNumberInvalid, partNumber)
	}
	return nil
}

// MultipartStagingDir 分片暂存目录。
//
// 以 "." 开头：它和正式对象共用 rootDir，而孤儿对账会扫整棵目录树。
// 这个前缀让 partDir 能被显式排除，否则正在上传的分片会被当成孤儿删掉。
const MultipartStagingDir = ".multipart"

// IsMultipartStaging 判断一个 object key 是否属于分片暂存区。
//
// 孤儿对账必须跳过它们：正在上传的分片没有任何资源记录指向，
// 在对账眼里和孤儿一模一样，而它们恰恰是最不该被删的东西。
func IsMultipartStaging(objectKey string) bool {
	return strings.HasPrefix(objectKey, MultipartStagingDir+"/")
}
