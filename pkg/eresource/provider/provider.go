package provider

import (
	"context"
	"errors"
	"io"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/internal/object"
)

// ValidateIdentitySegments 校验请求中参与路径与隔离判定的身份字段。
// Manager 在读取上传内容之前先调用它，避免为了一个非法的 namespace
// 白读一个几十 MB 的请求体。
func ValidateIdentitySegments(req *model.UploadRequest) error {
	_, err := object.IdentitySegments(req)
	return err
}

// 内置 Provider 类型标识
const (
	TypeLocal    = "local"
	TypeAliyunSS = "aliyun_oss"
)

type Provider interface {
	Name() string
	Upload(ctx context.Context, req *model.UploadRequest) (*model.ProviderResult, error)
	Delete(ctx context.Context, objectKey string) error
	Stat(ctx context.Context, objectKey string) (*model.ObjectStat, error)
	BuildURL(objectKey string, opts *model.URLBuildOption) (string, error)
	BuildSignedURL(ctx context.Context, objectKey string, opts *model.SignedURLBuildOption) (string, error)
}

// ObjectReader 由支持服务端直接读取对象内容的 Provider 实现。
// private 资源的代理下载依赖该能力：由业务服务读流回吐，不向客户端暴露底层地址。
// 未实现该接口的 Provider（如对象存储）应改用 302 跳转到签名地址。
type ObjectReader interface {
	Open(ctx context.Context, objectKey string) (io.ReadCloser, *model.ObjectStat, error)
}

// DirectUploader 由支持客户端直连上传的 Provider 实现。
//
// 签发的凭证必须把 object key、Content-Type 与体积上限一并锁死：
// 服务端在直传路径上看不到任何字节，这些条件是唯一能在对象存储侧
// 强制执行的约束。少锁一项，客户端就能拿着凭证往租户目录里
// 写任意大小、任意类型的对象。
//
// 本地盘无法实现该接口——直传的前提是存在一个客户端可直连的存储端点。
// 未实现时 Manager 返回 ErrDirectUploadUnsupported。
type DirectUploader interface {
	SignUploadPolicy(ctx context.Context, req *model.DirectUploadRequest) (*model.DirectUploadPolicy, error)
}

// ObjectWriter 由支持「按指定 object key 原样写入」的 Provider 实现。
//
// 与 Upload 的区别是**不生成 key**：副本同步必须落在与主存储
// 完全相同的 key 上，否则主备之间对不上号，恢复时每条记录
// 都要重新映射一次路径。而 Upload 会按自己的规则重新生成 key。
type ObjectWriter interface {
	PutObject(ctx context.Context, objectKey string, content []byte, contentType, accessPolicy string) error
}

// ErrObjectWriteUnsupported 当前 provider 不支持按指定 key 写入。
var ErrObjectWriteUnsupported = errors.New("eresource: provider does not support writing to an explicit object key")

// MaxSegmentLength 是对象路径中单个身份字段的最大长度。
const MaxSegmentLength = object.MaxSegmentLength

// ErrInvalidSegment 表示对象路径中的身份字段非法。
var ErrInvalidSegment = object.ErrInvalidSegment

// ErrProcessUnsupported 表示 Provider 不支持媒体处理。
var ErrProcessUnsupported = errors.New("eresource: provider does not support media processing")

// ACLSetter 由「对象自身带访问控制」的 Provider 实现。
//
// 访问策略在更新接口里是可以改的（草稿转发布是素材类业务的常规动作），
// 而对象存储的 ACL 是逐对象的：只改元数据里的 access_policy 字段，
// 得到的是一条「元数据说公开、对象还是私有」的资源，直链必然 403；
// 反向则更糟——元数据说私有，对象在 OSS 侧仍然人人可读。
//
// 本地盘刻意不实现：local 的受控访问靠验签路由把关，磁盘上的文件
// 没有「对象 ACL」这个概念，公开与否取决于它在不在静态目录下。
type ACLSetter interface {
	SetObjectACL(ctx context.Context, objectKey, accessPolicy string) error
}

// ObjectLister 由支持列举底层对象的 Provider 实现。
//
// 只有一个用途：对账。上传成功、登记失败（典型是进程在两步之间崩溃）
// 会留下没有任何记录指向的孤儿对象，它不占用任何功能，却一直在计费。
// 要发现它们，必须能从**对象这一侧**反查，而正常业务路径上
// 永远是从记录查对象，所以这个能力在主接口上没有位置。
//
// 分页是强制的：一个租户几十万对象不能一次拉回内存。
type ObjectLister interface {
	// ListObjects 列举 prefix 下的对象。
	//
	// marker 传空表示从头开始，返回的 next 非空时表示还有下一页，
	// 把它原样传回即可继续。limit <= 0 时由实现取一个合理默认值。
	ListObjects(ctx context.Context, prefix, marker string, limit int) (entries []model.ObjectEntry, next string, err error)
}

// RangeReader 由支持按字节区间读取对象的 Provider 实现。
//
// 没有它，受控的音视频就只能从头读到尾：浏览器拖一次进度条会重新发起一次
// 完整请求，OSS 场景下每一次 seek 都是一整个文件的回源流量。
//
// offset 是起始字节（含），length 是要读的字节数；length <= 0 表示读到末尾。
// 返回的 ObjectStat.Size 必须是**对象的完整大小**而不是本次区间的长度——
// 调用方要拿它拼 Content-Range 的分母。
type RangeReader interface {
	OpenRange(ctx context.Context, objectKey string, offset, length int64) (io.ReadCloser, *model.ObjectStat, error)
}

// 存储类型。冷数据转低频 / 归档能把存储单价降一个档，
// 代价是取回要收费（归档还要先解冻）。
const (
	StorageClassStandard = "Standard"
	StorageClassIA       = "IA"      // 低频访问
	StorageClassArchive  = "Archive" // 归档，取回前需解冻
)

// ErrStorageClassUnsupported 当前 provider 不支持存储分层。
var ErrStorageClassUnsupported = errors.New("eresource: provider does not support storage class transition")

// StorageClassSetter 由支持存储分层的 Provider 实现。
//
// 模块这边只提供「按条件把一批对象转到某个存储类型」这个动作。
// 真正的分层策略（多久没访问转低频、多久转归档）**更适合写成
// bucket 的生命周期规则**：那是对象存储原生的能力，由它执行
// 不占业务服务的任何资源，也不会因为业务进程没跑而停摆。
// 这个接口存在的意义是补上生命周期规则表达不了的部分——
// 按 biz_type、按租户这类只有元数据才知道的维度。
type StorageClassSetter interface {
	SetStorageClass(ctx context.Context, objectKey, storageClass string) error
}
