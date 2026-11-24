package model

import (
	"io"
)

// UploadRequest 发起资源上传的请求数据结构
type UploadRequest struct {
	FileName    string    // 原始文件名（带后缀扩展名）
	ContentType string    // MIME 类型，可选
	Reader      io.Reader // 文件读取流（流式上传时使用，与 Bytes 二选一，优先级更高）
	Bytes       []byte    // 文件二进制内容（全量内存上传时使用）
	// 若业务调用时未传递 Namespace 或 AccountID，算法会自动平滑跳过并合理缩进该物理前缀层级
	Namespace     string            // 业务命名空间/服务归属隔离，空采用默认
	AccountID     string            // SaaS 租户或账户归属硬隔离，空采用默认
	BizType       string            // 业务分类标识，用于隔离物理存储子目录
	Tags          []string          // 自定义分类标签属性列表
	AccessPolicy  string            // 访问策略模式（public/protected/private）
	ExpireSeconds int64             // 访问保护链接过期时间，单位：秒（针对 protected/private 场景）
	Creator       string            // 上传发起人标识
	Metadata      map[string]string // 极其灵活的额外自定义元数据属性 K-V 键值对

	// IdempotencyKey 可选的幂等键。同一租户下重复提交同一个键时，
	// 第二次及以后直接返回首次的结果，不会产生第二条记录与第二个对象。
	// 由调用方生成（典型是一个 UUID），并在整个重试过程中保持不变。
	IdempotencyKey string

	// Shared 请求把资源标记为平台级共享（跨租户只读）。
	// 只有管理员调用才会被采纳，普通调用方设置该字段会被静默忽略——
	// 否则任何业务都能把自己的资源变成全局可读。
	Shared bool
	// IsAdmin 调用方是否为管理员，决定 Shared 是否生效。
	// 与 AccessRequest.IsAdmin 同一套信任模型：必须由网关覆写。
	IsAdmin bool

	// 以下字段由 Manager 在校验阶段回填后再交给 Provider，调用方无需设置。
	Hash string // 文件内容 MD5 摘要，Provider 用它生成稳定且不冲突的 object key
	Ext  string // 归一化后的小写扩展名（含前导点，如 ".png"）
}

// UploadResult 资源上传成功后的统一返回结果
type UploadResult struct {
	Resource *Resource // 写入持久化成功的完整元数据信息
	Uploaded bool      // 是否实际执行了底层写入（命中去重/秒传时为 false）
	Existed  bool      // 是否命中历史重复资源记录（仅在开启 EnableDedup 时有效）
	URL      string    // 本次请求可用的对外访问地址（可能是公开直链，也可能是带时效的动态签名链接）
}

// View 返回可以安全交给客户端的副本，隐藏底层存储布局。
//
// 上传响应与 Get / List 一样会把整条元数据交给调用方，同样必须脱敏：
// store_path 与 bucket 泄露出去，等于把绕过验签所需的最后一块拼图给了对方。
func (r *UploadResult) View() *UploadResult {
	if r == nil {
		return nil
	}
	cloned := *r
	cloned.Resource = r.Resource.View()
	return &cloned
}

// AccessRequest 客户端请求获取或下载资源时的上下文校验信息。
//
// Namespace 与 AccountID 是租户隔离边界，必须由可信来源（网关注入的请求头、
// 会话信息）填充，绝不能取自客户端可自由指定的查询参数——否则调用方
// 只要换一个 account_id 就能读到别的租户的资源。
type AccessRequest struct {
	Namespace string // 调用方所属的业务命名空间，必须与资源一致
	AccountID string // 调用方所属的租户/账户，必须与资源一致
	UserID    string // 请求访问的用户 ID
	// IsAdmin 调用方是否为该租户的管理员。
	// 与 Namespace / AccountID 同一套信任模型：必须由网关或鉴权中间件
	// 依据会话覆写，不能取自客户端自带的请求头。
	// 当前只影响删除权限：管理员可以删除本租户内他人上传的资源，
	// 但仍然受 authorizeScope 的租户边界约束。
	IsAdmin bool
	// AdminBizTypes 把管理员权限限定在列出的 biz_type 内。
	//
	// 空切片表示不限定——保持 IsAdmin 原有的全租户语义，老接入方不受影响。
	// 非空时，管理员只能改动这些业务线下的资源：企业客户的常见要求是
	// 「素材运营能删素材，但不能删合同附件」，而 IsAdmin 这一档布尔
	// 表达不了这种区分。
	//
	// 与 IsAdmin 同一套信任模型：必须由网关 / 鉴权中间件按会话覆写。
	AdminBizTypes []string
	ClientIP      string // 客户端 IP（用于访问频率控制、黑白名单风控）
	UserAgent     string // 客户端浏览器 UA（用于审计与风控）
	Referer       string // 请求来源 Referer 头部（用于图片等资源的防盗链 Referer 白名单校验）
}
