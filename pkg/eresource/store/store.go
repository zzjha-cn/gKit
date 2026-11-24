package store

import (
	"context"
	"errors"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// ErrNotFound 统一的“记录不存在”哨兵错误。
// 各 Store 实现必须包装成该错误，调用方才能区分“查不到”与“存储不可用”。
var ErrNotFound = errors.New("eresource: resource not found")

type Store interface {
	Create(ctx context.Context, resource *model.Resource) error
	Update(ctx context.Context, resource *model.Resource) error
	// Patch 只写入补丁中非 nil 的字段，其余字段保持存储中的现值。
	//
	// 与 Update 并存而不是替代它：全量写回在「刚构造出一条完整记录」时最直接，
	// 而对一条已经存在、可能正被别的请求改动的记录，整条覆盖会把并发的
	// 改动（典型如一次软删除）一起抹掉。更新类操作一律走 Patch。
	// 记录不存在时返回 ErrNotFound。
	Patch(ctx context.Context, id string, patch *model.ResourcePatch) error
	Delete(ctx context.Context, id string) error
	GetByID(ctx context.Context, id string) (*model.Resource, error)
	GetByHash(ctx context.Context, hash string) (*model.Resource, error)
	List(ctx context.Context, query *model.Query) ([]*model.Resource, error)
	// Count 返回匹配条件的总数，忽略 Offset / Limit。后台分页据此展示总量。
	Count(ctx context.Context, query *model.Query) (int64, error)
}
