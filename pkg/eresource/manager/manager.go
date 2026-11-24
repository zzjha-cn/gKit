package manager

import (
	"context"
	"io"
	"net/url"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

type Manager interface {
	Upload(ctx context.Context, req *model.UploadRequest) (*model.UploadResult, error)
	// Get 读取元数据。req 承载调用方身份用于租户与访问策略校验，
	// 传 nil 表示可信的进程内调用。
	Get(ctx context.Context, id string, req *model.AccessRequest) (*model.Resource, error)
	GetAccessURL(ctx context.Context, id string, req *model.AccessRequest) (string, error)
	// Update 更新资源的业务元数据（展示名、分类、标签、metadata、访问策略）。
	// 存储布局、内容事实与身份归属不可改。req 语义同 Get。
	Update(ctx context.Context, id string, update *model.UpdateRequest, req *model.AccessRequest) (*model.Resource, error)
	VerifyAccess(ctx context.Context, id string, req *model.AccessRequest) error
	// GetVerified 一次查库完成「取元数据 + 完整访问校验」，
	// 等价于 Get + VerifyAccess 但只查一次库。受控下载路径使用。
	GetVerified(ctx context.Context, id string, req *model.AccessRequest) (*model.Resource, error)
	// Delete 删除资源。req 承载调用方身份用于租户与归属校验，
	// 传 nil 表示可信的进程内调用。
	Delete(ctx context.Context, id string, req *model.AccessRequest) error
	List(ctx context.Context, query *model.Query) ([]*model.Resource, error)
	// Count 返回匹配条件的资源总数，忽略分页参数。
	Count(ctx context.Context, query *model.Query) (int64, error)
	// Usage 读取租户的存储用量。未接用量存储时返回 (nil, nil)。
	// 计量口径见 model.Usage：按对象而不是按记录，软删不减、purge 才减。
	Usage(ctx context.Context, namespace, accountID string) (*model.Usage, error)

	// BatchAccessURL 批量签发访问地址，单条失败不影响整批。
	BatchAccessURL(ctx context.Context, ids []string, req *model.AccessRequest) ([]BatchResult, error)
	// BatchDelete 批量删除，单条失败不影响整批。
	BatchDelete(ctx context.Context, ids []string, req *model.AccessRequest) ([]BatchResult, error)

	// VerifySignedObject 校验签名地址的摘要、用户绑定与有效期。
	// requesterID 是请求方的真实身份，只在地址绑定了用户时参与校验。
	VerifySignedObject(objectKey string, query url.Values, requesterID string) error
	// AllowSignedAccess 对签名地址的下载做频控。必须在验签通过之后才调用。
	AllowSignedAccess(objectKey, clientIP string) error
	// OpenObject 读取底层对象内容，用于 private 资源的代理下载。
	OpenObject(ctx context.Context, objectKey string) (io.ReadCloser, *model.ObjectStat, error)
	// OpenObjectRange 按字节区间读取底层对象，支撑 Range 请求。
	// Provider 不支持时返回 ErrRangeUnsupported，调用方应退化成回吐整个对象。
	OpenObjectRange(ctx context.Context, objectKey string, offset, length int64) (io.ReadCloser, *model.ObjectStat, error)
	// StatObject 读取底层对象的大小与类型，不拉取内容，供 HEAD 请求使用。
	StatObject(ctx context.Context, objectKey string) (*model.ObjectStat, error)

	// ShouldRedirectContent 判断受控下载是否应当改走 302 分流（B12 的成本治理）。
	ShouldRedirectContent(resource *model.Resource) bool
	// RedirectURL 为 302 分流签发一条短时效地址。
	RedirectURL(ctx context.Context, resource *model.Resource, userID string) (string, error)
	// AuditDownload 记录一次下载审计。
	// 返回非 nil 表示审计拒绝了本次访问（仅 access.audit_fail_closed 打开时可能发生）。
	AuditDownload(ctx context.Context, resource *model.Resource, req *model.AccessRequest, route string, bytes int64) error
}
