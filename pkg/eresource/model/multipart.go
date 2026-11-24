package model

import (
	"time"
)

// 分片上传的规格约束。
//
// 下限 5MB 不是本模块的选择，是对象存储的硬性要求（除最后一片外，
// 每一片都不得小于 5MB）。把它写在这里而不是只写在 OSS 实现里，
// 是为了让本地 provider 的行为与线上一致——否则开发时能传的分片
// 到了线上会被 OSS 拒掉。
const (
	MinPartSize     int64 = 5 << 20   // 5MB，最后一片除外
	DefaultPartSize int64 = 8 << 20   // 建议分片大小
	MaxPartSize     int64 = 100 << 20 // 100MB
	MaxPartNumber         = 10000     // 单次上传的最大分片数
)

// MultipartInitRequest 发起分片上传的入参。
//
// 与 UploadRequest 的关键区别：没有内容，只有**声明的**总大小。
// 服务端据此做配额与体积校验，但真实大小要到 Complete 时才知道——
// 这是分片上传固有的信任缺口，不是实现偷懒。Complete 时会按
// 实际累计大小再校验一次。
type MultipartInitRequest struct {
	Namespace string
	AccountID string
	Creator   string

	FileName     string
	ContentType  string
	BizType      string
	AccessPolicy string
	Tags         []string
	Metadata     map[string]string

	// TotalSize 客户端声明的文件总大小，<=0 表示未知。
	TotalSize int64
	// PartSize 期望的分片大小，<=0 时由服务端给建议值。
	PartSize int64

	ExpireSeconds  int64
	IdempotencyKey string
	Shared         bool
	IsAdmin        bool
}

// MultipartSession 分片上传会话，交还给客户端后由它在后续请求里原样带回。
type MultipartSession struct {
	// Ticket 自包含的签名票据，承载 object key、uploadID 与身份。
	//
	// 与直传凭证同一套做法：服务端不存会话表。分片上传可能持续很久，
	// 也可能中途永远消失，为此维护一张需要清理的中间表并不划算。
	Ticket    string    `json:"ticket"`
	UploadID  string    `json:"upload_id"`
	PartSize  int64     `json:"part_size"`
	MaxParts  int       `json:"max_parts"`
	ExpireAt  time.Time `json:"expire_at"`
	ObjectKey string    `json:"-"` // 内部流转，不交给客户端
}

// MultipartPart 一个已上传分片的标识。
type MultipartPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size,omitempty"`
}

// MultipartInit 由 Provider 返回的分片上传初始化结果。
type MultipartInit struct {
	ObjectKey string
	UploadID  string
	Bucket    string
	StorePath string
	Provider  string
}

// MultipartCompleteRequest 完成分片上传的入参。
type MultipartCompleteRequest struct {
	Ticket    string
	Namespace string
	AccountID string
	UserID    string

	Parts         []MultipartPart
	Tags          []string
	Metadata      map[string]string
	ExpireSeconds int64
}
