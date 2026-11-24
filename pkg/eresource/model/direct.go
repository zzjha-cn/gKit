package model

import (
	"time"
)

// DirectUploadRequest 申请前端直传凭证的入参。
//
// 与 UploadRequest 的区别只有一个：没有文件内容。服务端在签发凭证时
// 看不到任何字节，因此所有内容相关的约束（体积、Content-Type、object key）
// 必须在这一步写进凭证本身，由对象存储在接收表单时强制执行。
type DirectUploadRequest struct {
	// 身份字段与普通上传同一套信任模型：只能来自 IdentityResolver 的结果，
	// 不能取自客户端表单——否则客户端换一个 account_id 就能往别的租户里塞文件。
	Namespace string
	AccountID string
	Creator   string

	BizType      string
	FileName     string            // 原始文件名，用于回填元数据与推断扩展名
	ContentType  string            // 客户端声明的 MIME 类型，会被写死进凭证条件
	AccessPolicy string            // 访问策略，决定对象 ACL
	Tags         []string          // 自定义标签，登记时写入元数据
	Metadata     map[string]string // 额外元数据，登记时写入

	// 以下字段由 Manager 回填后再交给 Provider，调用方无需设置。
	Ext           string // 归一化后的小写扩展名（含前导点）
	MaxSize       int64  // 允许上传的最大字节数
	ExpireSeconds int64  // 凭证有效期
}

// DirectUploadPolicy 签发给客户端的直传凭证。
//
// FormFields 必须原样作为表单字段提交，且文件字段（FileField）要放在最后——
// OSS 在读到文件内容时就开始校验前面的字段，顺序颠倒会直接被拒。
type DirectUploadPolicy struct {
	Provider   string            `json:"provider"`    // 签发凭证的存储提供商
	Method     string            `json:"method"`      // 表单提交方法，当前固定 POST
	UploadURL  string            `json:"upload_url"`  // 直传目标地址
	FileField  string            `json:"file_field"`  // 文件内容对应的表单字段名
	FormFields map[string]string `json:"form_fields"` // 需要原样带上的表单字段
	ObjectKey  string            `json:"object_key"`  // 本次凭证锁定的对象 key
	MaxSize    int64             `json:"max_size"`    // 允许的最大字节数
	ExpireAt   time.Time         `json:"expire_at"`   // 凭证失效时间
	Ticket     string            `json:"ticket"`      // 上传完成后登记元数据用的凭证，由 Manager 签发

	// Bucket 与 StorePath 只在服务端内部流转：它们会被签进 Ticket，
	// 登记时直接采用，避免再向 Provider 要一次底层布局信息。
	// 不序列化给客户端——这两项是 Resource.View() 一直在抹掉的东西。
	Bucket    string `json:"-"`
	StorePath string `json:"-"`
}

// DirectUploadCompleteRequest 直传完成后回来登记元数据的入参。
//
// 除了 Ticket 之外的身份字段同样来自 IdentityResolver：票据里签了一份身份，
// 这里再比对一次，防止 A 拿到 B 的票据去登记（票据是不记名的字符串，
// 泄露出去就是一次可用的写入许可）。
type DirectUploadCompleteRequest struct {
	Ticket    string
	Namespace string
	AccountID string
	Creator   string

	// FileName 可选：客户端在拿凭证之后才知道最终文件名时可以覆盖。
	// 不影响 object key——那个已经被凭证锁死了。
	FileName      string
	Tags          []string
	Metadata      map[string]string
	ExpireSeconds int64 // 登记后返回的访问地址有效期
}
