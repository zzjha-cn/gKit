package model

import (
	"time"
)

// 访问控制策略类型定义
const (
	AccessPolicyPublic    = "public"    // 永久公开可访问
	AccessPolicyProtected = "protected" // 受保护，访问时需要生成临时签名URL、校验Referer或频控
	AccessPolicyPrivate   = "private"   // 私有资源，不直接暴露存储地址，需要业务授权后通过代理流或重定向访问
)

// 资源元数据生命周期状态
const (
	ResourceStatusActive  = "active"  // 有效状态
	ResourceStatusDeleted = "deleted" // 已删除状态
)

// Clone 返回资源的深拷贝。
// Store 读写都走副本，避免调用方在锁外修改到共享状态。
func (r *Resource) Clone() *Resource {
	if r == nil {
		return nil
	}
	cloned := *r
	if r.Tags != nil {
		cloned.Tags = append([]string(nil), r.Tags...)
	}
	if r.Metadata != nil {
		cloned.Metadata = make(map[string]string, len(r.Metadata))
		for k, v := range r.Metadata {
			cloned.Metadata[k] = v
		}
	}
	return &cloned
}

// View 返回可以安全交给客户端的元数据副本。
//
// StorePath 是服务器本地绝对路径（OSS 下则是 bucket.endpoint/key），
// Bucket 直接点名底层桶，ObjectKey 则带着完整的租户 / 业务 / 年月分桶结构：
// 三者对调用方都没有用处（取内容用 id 走 /access 或 /content），
// 却把存储布局完整暴露出去，因此对外一律抹掉。
//
// 抹 ObjectKey 不等于它永远不会出现在客户端手里——protected / private 的
// 签名地址路径里就是 object key。但那是一条一条签出去的，
// 而 Get / List 是把整个租户的布局一次性交出去。
func (r *Resource) View() *Resource {
	if r == nil {
		return nil
	}
	cloned := r.Clone()
	cloned.StorePath = ""
	cloned.Bucket = ""
	cloned.ObjectKey = ""
	return cloned
}

// Resource 资源元数据领域模型
type Resource struct {
	ID                string    `json:"id" bson:"id"`                                                         // 资源唯一 ID
	Namespace         string    `json:"namespace,omitempty" bson:"namespace,omitempty"`                       // 业务命名空间/服务标识，用作全局逻辑分区（如 crm, im, algo）
	AccountID         string    `json:"account_id,omitempty" bson:"account_id,omitempty"`                     // SaaS 租户、组织或商家账户 ID，用于多租户硬数据隔离
	Name              string    `json:"name" bson:"name"`                                                     // 原始文件名
	Ext               string    `json:"ext" bson:"ext"`                                                       // 文件扩展名
	MimeType          string    `json:"mime_type" bson:"mime_type"`                                           // MIME 类型
	Size              int64     `json:"size" bson:"size"`                                                     // 文件大小，单位：字节
	Hash              string    `json:"hash" bson:"hash"`                                                     // 文件内容 MD5 摘要，用于秒传与去重
	Provider          string    `json:"provider" bson:"provider"`                                             // 存储提供商标识（如 local、aliyun_oss）
	Bucket            string    `json:"bucket,omitempty" bson:"bucket,omitempty"`                             // 存储 bucket（云存储有效，本地存储时为空）
	ObjectKey         string    `json:"object_key" bson:"object_key"`                                         // 对象存储 key 或相对路径
	StorePath         string    `json:"store_path,omitempty" bson:"store_path"`                               // 底层物理真实存储路径
	URL               string    `json:"url" bson:"url"`                                                       // 默认对外访问的 URL 地址
	Scheme            string    `json:"scheme" bson:"scheme"`                                                 // 访问协议（http 或 https）
	Status            string    `json:"status" bson:"status"`                                                 // 资源生命周期状态
	BizType           string    `json:"biz_type,omitempty" bson:"biz_type,omitempty"`                         // 业务分类标识，用于隔离存储路径
	Tags              []string  `json:"tags,omitempty" bson:"tags,omitempty"`                                 // 自定义分类标签属性
	AccessPolicy      string    `json:"access_policy" bson:"access_policy"`                                   // 访问控制策略（public/protected/private）
	HotlinkProtection bool      `json:"hotlink_protection" bson:"hotlink_protection"`                         // 是否启用防盗链策略（如 Referer 白名单校验）
	SignedURLExpireAt time.Time `json:"signed_url_expire_at,omitempty" bson:"signed_url_expire_at,omitempty"` // 签名 URL 过期时间
	Creator           string    `json:"creator,omitempty" bson:"creator,omitempty"`                           // 上传发起人标识
	// Shared 标记平台级共享资源：跨租户只读可见。
	//
	// 与 AccessPolicy 正交——共享的 private 资源仍然要签名才能取，
	// 放开的只是「谁能取」的租户边界。写入侧不受影响：
	// 共享资源的修改与删除仍然只有归属租户能做（见 authorizeMutate）。
	Shared bool `json:"shared,omitempty" bson:"shared,omitempty"`
	// IdempotencyKey 调用方声明的「同一次操作」标识，用于重试去重。
	// 与内容去重不是一回事：内容去重看哈希，幂等键看操作。
	IdempotencyKey string `json:"idempotency_key,omitempty" bson:"idempotency_key,omitempty"`
	// IdempotencyExpireAt 幂等键的失效时间。
	//
	// 过了这个点，后台任务会把 idempotency_key 字段**整个 unset**
	// 删的是键不是记录：资源本身要一直留着，过期的只是"这次重试还能命中"
	// 这个承诺。不加 TTL 的话，这个索引会随上传量无限增长。
	IdempotencyExpireAt time.Time `json:"-" bson:"idempotency_expire_at,omitempty"`

	DeletedAt time.Time `json:"deleted_at,omitempty" bson:"deleted_at,omitempty"`
	// Version 与 ParentID 为资源版本化预留。
	//
	// 当前不实现版本管理（见 Design.md 非目标），但位置必须现在留出：
	// 等到「换一版图、旧链接继续可用」成为需求时，Resource 上如果一个
	// 版本维度都没有，就只能新建记录再让业务自己串关系。
	// Version 从 1 起算，ParentID 指向同一资源的首个版本，单版本资源为空。
	Version   int               `json:"version,omitempty" bson:"version,omitempty"`
	ParentID  string            `json:"parent_id,omitempty" bson:"parent_id,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty" bson:"metadata,omitempty"` // 极其灵活的 K-V 动态业务扩展元数据，防频繁 Schema 变更
	CreatedAt time.Time         `json:"created_at" bson:"created_at"`                 // 资源创建时间
	UpdatedAt time.Time         `json:"updated_at" bson:"updated_at"`                 // 资源更新时间
}
