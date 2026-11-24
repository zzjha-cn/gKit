package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// ErrNoReplicaTarget 没有配置副本目标。
var ErrNoReplicaTarget = errors.New("eresource: no replica provider configured")

// ReplicaResult 一次副本同步的结果。
type ReplicaResult struct {
	Scanned int      // 扫过的资源数
	Copied  int      // 实际复制的对象数
	Skipped int      // 副本侧已存在且大小一致，跳过
	Missing []string // 源对象已经不在了的资源 ID
}

// SetReplicaProvider 注入副本目标。约定在启动期调用。
func (m *DefaultManager) SetReplicaProvider(p provider.Provider) {
	if p == nil {
		m.replica.Store((*provider.Provider)(nil))
		return
	}
	m.replica.Store(&p)
}

func (m *DefaultManager) replicaProvider() provider.Provider {
	stored, _ := m.replica.Load().(*provider.Provider)
	if stored == nil {
		return nil
	}
	return *stored
}

// Replicate 把匹配条件的对象镜像到副本存储。
//
// 为什么这件事值得写成代码：bucket 原生的跨区域复制更可靠、不占业务资源，
// 有条件就该优先用它。但它覆盖不到三种情况——复制到**另一个厂商**、
// 复制到本地盘做离线备份、以及**只复制某些 biz_type**。
// 这三种都只有拿着元数据才做得到，而元数据在这边。
//
// 幂等：副本侧已存在且大小一致就跳过，可以反复跑。
// 单条失败不中断整批，返回已完成的部分与合并后的错误。
func (m *DefaultManager) Replicate(ctx context.Context, query *model.Query) (*ReplicaResult, error) {
	target := m.replicaProvider()
	if target == nil {
		return nil, ErrNoReplicaTarget
	}
	reader, ok := m.provider.(provider.ObjectReader)
	if !ok {
		// 读不出源对象就谈不上复制。OSS 与本地盘都实现了 ObjectReader，
		// 走到这里说明换了一个自定义 provider。
		return nil, ErrProxyDownloadUnsupported
	}
	if query == nil {
		query = &model.Query{}
	}
	// 只同步 active：已软删的资源正在等保留期到期被删掉，
	// 把它们复制到副本只是把一份即将消失的数据搬到另一个地方继续收费。
	query.Status = model.ResourceStatusActive

	resources, err := m.store.List(ctx, query)
	if err != nil {
		return nil, err
	}
	result := &ReplicaResult{Scanned: len(resources)}
	seen := make(map[string]struct{}, len(resources))
	var errs []error
	for _, resource := range resources {
		// 去重策略下多条记录共享一个对象，复制一次就够了。
		if _, done := seen[resource.ObjectKey]; done {
			continue
		}
		seen[resource.ObjectKey] = struct{}{}

		status, copyErr := m.replicateOne(ctx, reader, target, resource)
		switch {
		case copyErr != nil:
			errs = append(errs, fmt.Errorf("replicate %s: %w", resource.ID, copyErr))
		case status == replicaMissing:
			result.Missing = append(result.Missing, resource.ID)
		case status == replicaSkipped:
			result.Skipped++
		default:
			result.Copied++
		}
	}
	return result, errors.Join(errs...)
}

type replicaStatus int

const (
	replicaCopied replicaStatus = iota
	replicaSkipped
	replicaMissing
)

func (m *DefaultManager) replicateOne(ctx context.Context, reader provider.ObjectReader,
	target provider.Provider, resource *model.Resource) (replicaStatus, error) {
	// 先问副本侧：已经有一份大小一致的就跳过。
	// 按大小而不是按哈希比对，是因为源对象的哈希不一定有
	// （直传与分片上传的 Hash 字段是空的），而再拉一遍算哈希
	// 等于把"跳过"的开销变得和"复制"一样大。
	if stat, err := target.Stat(ctx, resource.ObjectKey); err == nil &&
		stat != nil && stat.Exists && stat.Size == resource.Size {
		return replicaSkipped, nil
	}
	body, stat, err := reader.Open(ctx, resource.ObjectKey)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 源对象没了。这是反向对账该管的事（ReconcileMissing），
			// 复制这边只如实报告，绝不把"源没了"同步成"副本也删掉"——
			// 那会把一次误删放大成两次。
			return replicaMissing, nil
		}
		return replicaCopied, err
	}
	defer body.Close()

	content, err := io.ReadAll(body)
	if err != nil {
		return replicaCopied, err
	}
	contentType := resource.MimeType
	if contentType == "" && stat != nil {
		contentType = stat.ContentType
	}
	// 走 PutObjectKey 而不是 Upload：副本必须落在**完全相同的 object key** 上，
	// 否则主备之间对不上号，恢复时每条记录都要重新映射一次路径。
	// 而 Upload 会按自己的规则重新生成 key。
	writer, ok := target.(provider.ObjectWriter)
	if !ok {
		return replicaCopied, provider.ErrObjectWriteUnsupported
	}
	if err = writer.PutObject(ctx, resource.ObjectKey, content, contentType, resource.AccessPolicy); err != nil {
		return replicaCopied, err
	}
	return replicaCopied, nil
}

// VerifyReplica 核对副本是否齐全，只报告不修复。
//
// 与 Replicate 分开，是因为「副本缺了什么」这个问题的答案
// 经常需要人看一眼再决定——缺了一万条可能意味着副本桶被误删，
// 这时候无脑重传一万个对象不如先停下来问问发生了什么。
func (m *DefaultManager) VerifyReplica(ctx context.Context, query *model.Query) ([]string, error) {
	target := m.replicaProvider()
	if target == nil {
		return nil, ErrNoReplicaTarget
	}
	if query == nil {
		query = &model.Query{}
	}
	query.Status = model.ResourceStatusActive
	resources, err := m.store.List(ctx, query)
	if err != nil {
		return nil, err
	}
	var missing []string
	seen := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		if _, done := seen[resource.ObjectKey]; done {
			continue
		}
		seen[resource.ObjectKey] = struct{}{}
		stat, statErr := target.Stat(ctx, resource.ObjectKey)
		if statErr != nil || stat == nil || !stat.Exists || stat.Size != resource.Size {
			missing = append(missing, resource.ID)
		}
	}
	return missing, nil
}
