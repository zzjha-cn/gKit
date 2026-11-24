package store

import (
	"context"
	"errors"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// ErrDuplicate 唯一约束冲突。幂等键重复时由 Create 返回。
//
// 单独立一个哨兵而不是把驱动错误透出去：调用方要据此做的是
// 「回查一次已有记录并返回」，而不是报错，两者的分支必须可区分。
var ErrDuplicate = errors.New("eresource: duplicate key")

// QuotaStore 维护按租户的存储用量。
//
// 与 Store 分开而不是并进去：用量是一份独立的聚合数据，
// 生命周期、索引、甚至存储介质都可以和资源元数据不同
// （典型做法是把它放进 Redis）。合进 Store 会强迫每个
// Store 实现都操心配额，而大多数实现根本不需要。
//
// 计量口径见 model.Usage 的注释——按对象而不是按记录，
// 软删除不减、真正 purge 才减。
type QuotaStore interface {
	// AddUsage 增量调整用量，bytes / files 可为负数。
	// 记录不存在时按零值创建。
	AddUsage(ctx context.Context, namespace, accountID string, bytes, files int64) error
	// GetUsage 读取当前用量。从未记录过的租户返回零值而不是 ErrNotFound——
	// 「没用过」和「用了 0 字节」对限额判断是同一件事。
	GetUsage(ctx context.Context, namespace, accountID string) (*model.Usage, error)
	// ComputeUsage 从资源记录现算一遍真实用量，不读增量计数。
	//
	// 这是对账的基准。增量计数会漂移——一次 $inc 失败、一次进程在
	// store.Create 与 AddUsage 之间崩溃，账面就和事实差开了，
	// 而这种偏差只会累积不会自愈。
	//
	// 口径必须与 model.Usage 一致：按 distinct(object_key) 聚合，
	// 已软删的记录仍然计入（它们的对象还在底层占着空间）。
	ComputeUsage(ctx context.Context, namespace, accountID string) (*model.Usage, error)
}
