package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Query 资源元数据组合检索条件结构体
type Query struct {
	IDs          []string // 指定检索的目标资源 ID 集合
	Namespace    string   // 筛选业务命名空间隔离区
	AccountID    string   // 筛选具体的多租户/账户归属
	Hash         string   // 按文件内容哈希进行精确查询（秒传检验）
	Provider     string   // 筛选底层存储介质（如 local, aliyun_oss）
	ObjectKey    string   // 按底层对象 key 精确查询，用于判断一个对象还有几条记录在引用
	BizType      string   // 筛选归属业务类型
	Status       string   // 筛选资源当前状态（active/deleted）
	AccessPolicy string   // 筛选访问策略（public/protected/private）
	Creator      string   // 筛选上传人标识
	// IdempotencyKey 按幂等键精确查询，用于重试命中判定。
	IdempotencyKey string
	// IdempotencyExpiredBefore 筛选"带着幂等键、且该键已在此时刻之前失效"的记录。
	// 只供过期清理任务使用。
	IdempotencyExpiredBefore time.Time
	// Shared 按共享标记过滤。nil 表示不限定；取 false 时只返回非共享资源。
	Shared        *bool
	CreatedAfter  time.Time // 筛选创建时间大于或等于此时间的记录
	DeletedBefore time.Time // 筛选删除时间严格早于此时间的记录，兼容历史记录的 UpdatedAt
	CreatedBefore time.Time // 筛选创建时间小于或等于此时间的记录

	// Tags 标签过滤，AND 语义：资源必须同时带上列出的全部标签。
	// 用 AND 而不是 OR：标签检索的典型用法是逐个条件收窄结果集，
	// OR 语义下多加一个标签反而会让结果变多，与直觉相反。
	Tags []string
	// Metadata 业务扩展字段过滤，只支持精确等值，多个 key 之间是 AND。
	// 刻意不支持前缀 / 范围 / 正则：metadata 是无 schema 的自由字段，
	// 开放模糊匹配等于允许调用方构造任意一条无法走索引的全表扫描。
	Metadata map[string]string

	Offset int64 // 分页偏移起始位置
	Limit  int64 // 分页返回最大条数
}

// 检索条件的规模上限。
//
// 这两个值不是性能调优，而是防止单个请求构造出代价不可控的查询：
// metadata 的每个条件在 Mongo 里都是一个独立的字段匹配，没有索引兜底。
const (
	MaxQueryTags     = 8
	MaxQueryMetadata = 8
	// MaxQueryIDs 单次按 ID 批量检索的上限，与批量接口共用同一个口径。
	MaxQueryIDs = 100
)

// ErrInvalidQuery 检索条件非法。调用方应当据此回 400 而不是 500。
var ErrInvalidQuery = errors.New("eresource: invalid query condition")

// Validate 校验检索条件的合法性与规模。
//
// metadata 的 key 会被直接拼成存储层的字段路径（Mongo 下是 "metadata.<key>"），
// 因此必须在这里挡住 "$" 与 "."：前者能把一个等值条件变成任意查询操作符，
// 后者能越过 metadata 这一层去匹配文档里的其它字段。
func (q *Query) Validate() error {
	if q == nil {
		return nil
	}
	if len(q.IDs) > MaxQueryIDs {
		return fmt.Errorf("%w: at most %d ids per query", ErrInvalidQuery, MaxQueryIDs)
	}
	if len(q.Tags) > MaxQueryTags {
		return fmt.Errorf("%w: at most %d tags per query", ErrInvalidQuery, MaxQueryTags)
	}
	for _, tag := range q.Tags {
		if strings.TrimSpace(tag) == "" {
			return fmt.Errorf("%w: empty tag", ErrInvalidQuery)
		}
	}
	if len(q.Metadata) > MaxQueryMetadata {
		return fmt.Errorf("%w: at most %d metadata conditions per query", ErrInvalidQuery, MaxQueryMetadata)
	}
	for key := range q.Metadata {
		if err := ValidateMetadataKey(key); err != nil {
			return err
		}
	}
	if !q.CreatedAfter.IsZero() && !q.CreatedBefore.IsZero() && q.CreatedAfter.After(q.CreatedBefore) {
		return fmt.Errorf("%w: created_after is later than created_before", ErrInvalidQuery)
	}
	return nil
}

// MaxMetadataKeyLength metadata key 的长度上限。
const MaxMetadataKeyLength = 64

// ValidateMetadataKey 校验 metadata 的 key 是否可以安全地参与检索。
//
// 写入侧同样要用它：一条带着 "$gt" 作为 key 的记录即便查不出来，
// 也已经让存储层的文档结构变得不可预期。
func ValidateMetadataKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty metadata key", ErrInvalidQuery)
	}
	if len(key) > MaxMetadataKeyLength {
		return fmt.Errorf("%w: metadata key %q is too long", ErrInvalidQuery, key)
	}
	if strings.HasPrefix(key, "$") || strings.Contains(key, ".") {
		return fmt.Errorf("%w: metadata key %q must not contain '.' or start with '$'", ErrInvalidQuery, key)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: metadata key contains control characters", ErrInvalidQuery)
		}
	}
	return nil
}

// ProviderResult 物理介质写入成功后底层 Provider 返回的标准化结果
type ProviderResult struct {
	Provider  string // 具体写入的物理介质服务商名称（如 local, aliyun_oss）
	Bucket    string // 对象存储中的 bucket 桶名称（本地落盘时为空）
	ObjectKey string // 底层保存的唯一对象 Key 或文件相对路径
	StorePath string // 物理存储文件的全路径（本地绝对路径，或 OSS 的逻辑存储路径）
	ETag      string // 底层物理对象的 ETag 或校验和
	Size      int64  // 物理写入的文件实际大小，单位：字节
}

// ObjectStat 物理存储中文件的实体状态与描述信息
type ObjectStat struct {
	Exists      bool      // 对象在底层介质中是否存在
	Size        int64     // 文件的物理大小
	ContentType string    // 文件 MIME 格式
	UpdatedAt   time.Time // 文件实体最后的物理修改时间
}

// URLBuildOption 构建普通/公开访问外链的参数
type URLBuildOption struct {
	Scheme string // 协议，为空时使用全局默认值（如 http/https）
	Host   string // 域名/主机名，为空时使用全局默认域名
	// Process 媒体处理参数（缩略图、水印、转码等），留空表示取原图。
	//
	// 内容由底层能力定义（OSS 下是 x-oss-process 的取值），模块不解析它。
	// 刻意不做成结构化的「宽 / 高 / 质量」：各家处理服务的参数体系差异很大，
	// 硬抽象一层只会同时丢掉表达力和准确性。
	Process string
	// 这里没有 Path：base_path 是存储前缀，已经包含在 object key 里，
	// 不能在拼 URL 时再加一次，否则直链会多出一段指不到任何对象的路径。
}

// SignedURLBuildOption 构建动态受控时效安全外链的参数
type SignedURLBuildOption struct {
	ExpireSeconds int64  // 动态外链生成的安全存活时效，单位：秒
	Scheme        string // 协议
	Host          string // 域名
	Path          string // 路径前缀
	UserID        string // 关联当前请求的用户 ID，用于更深度的访问审计
	SecretKey     string // 动态签名使用的对称加密盐/盐值

	// Process 媒体处理参数。必须参与签名——附加在已签好的地址后面
	// 会让签名失效（OSS 把它算进 canonical query）。
	Process string

	// BindUser 要求请求方身份与 UserID 一致才放行，地址转发给别人即失效。
	// 仅对本地 provider 的验签路由有效：OSS 的签名地址直连 OSS，
	// 不经过本服务，无从校验请求方身份。
	BindUser bool
}

// URLSignOption 构建受控访问地址的参数，由 Manager 的签名模式实现消费。
//
// 与 SignedURLBuildOption 的区别：这一层用**绝对**过期时间而不是相对秒数。
// 过期时间窗对齐（CDN 可缓存的前提）只能在绝对时间上做，
// 相对秒数经过一次 now 之后就不再稳定了。
type URLSignOption struct {
	ExpireAt int64  // 绝对过期时间戳（秒）
	UserID   string // 关联的用户标识
	BindUser bool   // 是否把地址绑定到 UserID，仅本地验签路由支持
	Scheme   string // 协议
	Host     string // 域名；留空由实现自行回落
	Process  string // 媒体处理参数，留空表示取原图
}

// ObjectEntry 列举底层对象时的一条记录。
//
// 与 ObjectStat 分开：Stat 回答「这一个对象怎么样」，Entry 回答
// 「这批对象都有谁」，后者必须带上 key 本身。
type ObjectEntry struct {
	Key       string    // 对象 key 或相对路径
	Size      int64     // 对象大小
	UpdatedAt time.Time // 最后修改时间，孤儿判定的安全窗口依赖它
}
