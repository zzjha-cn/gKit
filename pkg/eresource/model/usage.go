package model

import (
	"time"
)

// Usage 单个租户的存储用量。
//
// 口径（这是配额治理里比技术选型更重要的决定，见 Roadmap.md §5）：
//
//   - 按**对象**而不是按记录计量。去重策略 new_record 下 N 条记录共享
//     一个对象，按记录累加 Size 会把同一份字节收 N 次钱。
//   - 软删除**不减**用量。保留期内对象还在底层占着真实存储，
//     这时候把用量减掉，账面与账单就对不上了。真正 purge 掉才减。
//   - Files 统计的同样是对象数，不是记录数。
type Usage struct {
	Namespace string    `json:"namespace" bson:"namespace"`
	AccountID string    `json:"account_id" bson:"account_id"`
	Bytes     int64     `json:"bytes" bson:"bytes"` // 已占用的字节数
	Files     int64     `json:"files" bson:"files"` // 已占用的对象数
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}
