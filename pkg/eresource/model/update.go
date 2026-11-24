package model

import (
	"time"
)

// UpdateRequest 更新资源元数据的请求。
//
// 只收可变的业务字段：存储布局（provider / bucket / object_key / store_path）、
// 内容事实（hash / size / mime_type / ext）与身份归属（namespace / account_id /
// creator）一律不可改——它们描述的是「这个对象是什么、属于谁」，
// 改了就等于让元数据与底层对象对不上。
//
// 指针与 nil 切片都表示「本次不改这个字段」，必须和「改成空值」区分开：
// 用零值表示不改的话，调用方永远没办法清空一个字段。
type UpdateRequest struct {
	// Name 展示用文件名。为空串会被拒绝（文件名是下载时的 Content-Disposition 来源）。
	Name *string
	// BizType 业务分类。只改元数据分类，不会搬动已经落盘的对象，
	// 因此改完之后物理路径里的 biz_type 仍是上传时那个值。
	BizType *string
	// AccessPolicy 访问策略。改动会连带调整底层对象 ACL 与 URL 字段，
	// 是本接口里唯一有安全后果的字段，权限与删除同级。
	AccessPolicy *string
	// Tags 标签，整体替换语义：传 nil 不改，传空切片清空。
	// 标签是一个集合，增量改的语义（加哪个、删哪个）比整体替换更难用也更难解释。
	Tags []string
	// Metadata 业务扩展字段，合并语义：只影响传进来的 key，值为空串表示删除该 key。
	// 这里刻意不用整体替换：调用方拿一份可能已经过期的全量 map 回写，
	// 会把并发写入的其它 key 一起抹掉。
	Metadata map[string]string
}

// IsEmpty 判断这次更新是否什么都没带。
func (r *UpdateRequest) IsEmpty() bool {
	if r == nil {
		return true
	}
	return r.Name == nil && r.BizType == nil && r.AccessPolicy == nil &&
		r.Tags == nil && r.Metadata == nil
}

// ResourcePatch 是 Store 侧的局部更新指令：只有非 nil 的字段参与写入。
//
// 存在的理由是避免「读出整条 → 改几个字段 → 整条写回」。那种做法下，
// 两个并发请求各改各的字段，后写的会把先写的改动整条覆盖掉；
// 更糟的是一次更新能把并发发生的软删除（status=deleted）直接改回 active。
type ResourcePatch struct {
	Name         *string
	BizType      *string
	AccessPolicy *string
	Status       *string
	// URL 公开直链。只有 public 资源才持久化它，
	// 策略改成非 public 时必须显式写回空串，而不是留着旧地址。
	URL *string
	// Tags 为 nil 表示不改；非 nil（含空切片）表示整体替换。
	Tags []string
	// Metadata 为 nil 表示不改；非 nil（含空 map）表示整体替换。
	// 合并逻辑在 Manager 侧完成，Store 只负责落最终值。
	Metadata map[string]string
	// ClearIdempotencyKey 为 true 时把 idempotency_key 与
	// idempotency_expire_at 两个字段整个移除（Mongo 下是 $unset）。
	//
	// 做成布尔而不是 *string：置空串会被唯一稀疏索引当成一个真实取值，
	// 所有过期记录会因为"都等于空串"而互相冲突。这个区别没法靠
	// 一个字符串指针表达，所以单独开一个字段把意图写死。
	ClearIdempotencyKey bool
	// UpdatedAt 为零值时由 Store 填当前时间。
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// IsEmpty 判断补丁是否不含任何字段改动。
func (p *ResourcePatch) IsEmpty() bool {
	if p == nil {
		return true
	}
	return p.Name == nil && p.BizType == nil && p.AccessPolicy == nil &&
		p.Status == nil && p.URL == nil && p.Tags == nil && p.Metadata == nil &&
		!p.ClearIdempotencyKey && p.DeletedAt == nil
}

// Apply 把补丁作用到一条资源上，供内存型 Store 使用。
func (p *ResourcePatch) Apply(r *Resource) {
	if p == nil || r == nil {
		return
	}
	if p.Name != nil {
		r.Name = *p.Name
	}
	if p.BizType != nil {
		r.BizType = *p.BizType
	}
	if p.AccessPolicy != nil {
		r.AccessPolicy = *p.AccessPolicy
	}
	if p.DeletedAt != nil {
		r.DeletedAt = *p.DeletedAt
	}
	if p.Status != nil {
		r.Status = *p.Status
	}
	if p.ClearIdempotencyKey {
		r.IdempotencyKey = ""
		r.IdempotencyExpireAt = time.Time{}
	}
	if p.URL != nil {
		r.URL = *p.URL
	}
	if p.Tags != nil {
		r.Tags = append([]string(nil), p.Tags...)
	}
	if p.Metadata != nil {
		metadata := make(map[string]string, len(p.Metadata))
		for k, v := range p.Metadata {
			metadata[k] = v
		}
		r.Metadata = metadata
	}
	r.UpdatedAt = p.UpdatedAt
}
