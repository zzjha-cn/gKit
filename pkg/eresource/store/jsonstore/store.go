package jsonstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

// JSONStore 本地 JSON 文件存储元数据实现（主要用于本地开发、单机轻量部署与离线工具场景）
type JSONStore struct {
	filePath  string                     // JSON 物理文件路径
	mu        sync.RWMutex               // 读写锁，保障内存 Map 并发安全
	resources map[string]*model.Resource // 内存索引 Map (ID -> Resource)
}

// NewJSONStore 构造并初始化本地 JSON Store，启动时会自动执行“启动加载”读取历史数据
func NewJSONStore(filePath string) (*JSONStore, error) {
	if filePath == "" {
		return nil, fmt.Errorf("json store file path is empty")
	}
	s := &JSONStore{
		filePath:  filePath,
		resources: make(map[string]*model.Resource),
	}
	// 执行启动加载，反序列化数据到内存
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("failed to load json store: %w", err)
	}
	return s, nil
}

// load 执行“启动加载”
func (s *JSONStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 检查元数据文件是否存在，如果不存在则自动创建父级目录并初始化空文件
	if _, err := os.Stat(s.filePath); os.IsNotExist(err) {
		dir := filepath.Dir(s.filePath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		return s.saveLocked()
	}

	file, err := os.Open(s.filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}

	if len(data) == 0 {
		return nil
	}

	var list []*model.Resource
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}

	// 载入内存 Map
	for _, r := range list {
		if r != nil && r.ID != "" {
			s.resources[r.ID] = r
		}
	}
	return nil
}

// saveLocked 执行“落盘”写入（此方法在持有写锁 mu 时调用）
// 采用临时文件重命名的“原子安全写入”策略，防止写到一半断电或崩溃导致原文件损坏
func (s *JSONStore) saveLocked() error {
	list := make([]*model.Resource, 0, len(s.resources))
	for _, r := range s.resources {
		list = append(list, r)
	}

	// 美化输出，方便本地开发环境手动查看和调试元数据
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := s.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, s.filePath)
}

// Create 元数据内存新增并落盘
func (s *JSONStore) Create(ctx context.Context, resource *model.Resource) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.resources[resource.ID]; exists {
		return fmt.Errorf("%w: id=%s", store.ErrDuplicate, resource.ID)
	}
	// JSONStore 没有唯一索引，幂等键的唯一性只能在写锁内自己扫一遍。
	// 记录数小，代价可以接受；语义必须和 MongoStore 的唯一索引一致，
	// 否则同一段 Manager 代码在两种 Store 下行为不同。
	if resource.IdempotencyKey != "" {
		for _, existing := range s.resources {
			if existing.IdempotencyKey == resource.IdempotencyKey &&
				existing.Namespace == resource.Namespace &&
				existing.AccountID == resource.AccountID {
				return fmt.Errorf("%w: idempotency_key=%s", store.ErrDuplicate, resource.IdempotencyKey)
			}
		}
	}

	// 存副本：调用方后续修改入参不应影响 store 内的状态
	s.resources[resource.ID] = resource.Clone()
	if err := s.saveLocked(); err != nil {
		delete(s.resources, resource.ID)
		return err
	}
	return nil
}

// Update 元数据内存更新并落盘
func (s *JSONStore) Update(ctx context.Context, resource *model.Resource) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	previous, exists := s.resources[resource.ID]
	if !exists {
		return fmt.Errorf("%w: id=%s", store.ErrNotFound, resource.ID)
	}

	resource.UpdatedAt = time.Now()
	s.resources[resource.ID] = resource.Clone()
	if err := s.saveLocked(); err != nil {
		s.resources[resource.ID] = previous
		return err
	}
	return nil
}

// Patch 局部更新内存记录并落盘。
//
// 整个过程在写锁内完成，因此「读出现值 → 作用补丁 → 写回」对调用方是原子的：
// 两个并发 Patch 各改各的字段不会互相覆盖。
func (s *JSONStore) Patch(ctx context.Context, id string, patch *model.ResourcePatch) error {
	_ = ctx
	if patch == nil || patch.IsEmpty() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	current, exists := s.resources[id]
	if !exists {
		return fmt.Errorf("%w: id=%s", store.ErrNotFound, id)
	}
	updated := current.Clone()
	applied := *patch
	if applied.UpdatedAt.IsZero() {
		applied.UpdatedAt = time.Now()
	}
	applied.Apply(updated)
	s.resources[id] = updated
	if err := s.saveLocked(); err != nil {
		s.resources[id] = current
		return err
	}
	return nil
}

// Delete 元数据内存删除并落盘
func (s *JSONStore) Delete(ctx context.Context, id string) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	previous, exists := s.resources[id]
	if !exists {
		return fmt.Errorf("%w: id=%s", store.ErrNotFound, id)
	}

	delete(s.resources, id)
	if err := s.saveLocked(); err != nil {
		s.resources[id] = previous
		return err
	}
	return nil
}

// GetByID 从内存 Map 高效查询一条记录
func (s *JSONStore) GetByID(ctx context.Context, id string) (*model.Resource, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	r, exists := s.resources[id]
	if !exists {
		return nil, fmt.Errorf("%w: id=%s", store.ErrNotFound, id)
	}
	// 返回副本：调用方在锁外修改不得污染共享状态
	return r.Clone(), nil
}

// GetByHash 从内存中线性匹配检索哈希命中记录（去重秒传检测）
func (s *JSONStore) GetByHash(ctx context.Context, hash string) (*model.Resource, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 命中多条时取最新的一条，与 MongoStore 行为对齐
	var newest *model.Resource
	for _, r := range s.resources {
		if r.Hash != hash || r.Status != model.ResourceStatusActive {
			continue
		}
		if newest == nil || r.CreatedAt.After(newest.CreatedAt) {
			newest = r
		}
	}
	if newest != nil {
		return newest.Clone(), nil
	}
	return nil, fmt.Errorf("%w: hash=%s", store.ErrNotFound, hash)
}

// List 条件组合过滤查询并支持分页
func (s *JSONStore) List(ctx context.Context, query *model.Query) ([]*model.Resource, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	var filtered []*model.Resource
	for _, r := range s.resources {
		if s.matchQuery(r, query) {
			filtered = append(filtered, r.Clone())
		}
	}

	// 排序：按 CreatedAt 降序（最新的在最前面），保证与 Mongo 默认列表体验一致
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].CreatedAt.After(filtered[j].CreatedAt)
	})

	// 分页拦截
	if query != nil {
		offset := query.Offset
		limit := query.Limit
		total := int64(len(filtered))

		if offset >= total {
			return []*model.Resource{}, nil
		}

		end := offset + limit
		if limit <= 0 || end > total {
			end = total
		}
		filtered = filtered[offset:end]
	}

	return filtered, nil
}

// Count 返回匹配条件的总数，忽略分页参数
func (s *JSONStore) Count(ctx context.Context, query *model.Query) (int64, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total int64
	for _, r := range s.resources {
		if s.matchQuery(r, query) {
			total++
		}
	}
	return total, nil
}

// matchQuery 条件匹配核心逻辑
func (s *JSONStore) matchQuery(r *model.Resource, q *model.Query) bool {
	if q == nil {
		return true
	}
	if len(q.IDs) > 0 {
		found := false
		for _, id := range q.IDs {
			if r.ID == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !q.DeletedBefore.IsZero() {
		deletedAt := r.DeletedAt
		if deletedAt.IsZero() {
			deletedAt = r.UpdatedAt
		}
		if !deletedAt.Before(q.DeletedBefore) {
			return false
		}
	}
	if q.Namespace != "" && r.Namespace != q.Namespace {
		return false
	}
	if q.AccountID != "" && r.AccountID != q.AccountID {
		return false
	}
	if q.Hash != "" && r.Hash != q.Hash {
		return false
	}
	if q.Provider != "" && r.Provider != q.Provider {
		return false
	}
	if q.ObjectKey != "" && r.ObjectKey != q.ObjectKey {
		return false
	}
	if q.BizType != "" && r.BizType != q.BizType {
		return false
	}
	if q.Status != "" && r.Status != q.Status {
		return false
	}
	if q.AccessPolicy != "" && r.AccessPolicy != q.AccessPolicy {
		return false
	}
	if q.Creator != "" && r.Creator != q.Creator {
		return false
	}
	if q.IdempotencyKey != "" && r.IdempotencyKey != q.IdempotencyKey {
		return false
	}
	if !q.IdempotencyExpiredBefore.IsZero() {
		if r.IdempotencyKey == "" {
			return false
		}
		if r.IdempotencyExpireAt.IsZero() || !r.IdempotencyExpireAt.Before(q.IdempotencyExpiredBefore) {
			return false
		}
	}
	if q.Shared != nil && r.Shared != *q.Shared {
		return false
	}
	if !matchTags(r.Tags, q.Tags) {
		return false
	}
	for key, value := range q.Metadata {
		if r.Metadata[key] != value {
			return false
		}
	}
	if !q.CreatedAfter.IsZero() && r.CreatedAt.Before(q.CreatedAfter) {
		return false
	}
	if !q.CreatedBefore.IsZero() && r.CreatedAt.After(q.CreatedBefore) {
		return false
	}
	return true
}

// matchTags 判断资源是否带齐了全部待匹配标签（AND 语义），与 MongoStore 的 $all 对齐。
func matchTags(resourceTags, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	owned := make(map[string]struct{}, len(resourceTags))
	for _, tag := range resourceTags {
		owned[tag] = struct{}{}
	}
	for _, tag := range wanted {
		if _, ok := owned[tag]; !ok {
			return false
		}
	}
	return true
}

// AddUsage 在 JSONStore 下是空操作。
//
// 这个实现刻意不维护增量计数：GetUsage 直接扫全量算精确值，
// 没有需要被增量修正的状态。dev / 单测场景记录数很小，
// 一次扫描比维护一份可能漂移的计数更可信。
func (s *JSONStore) AddUsage(ctx context.Context, namespace, accountID string, bytes, files int64) error {
	_, _, _, _, _ = ctx, namespace, accountID, bytes, files
	return nil
}

// ComputeUsage 与 GetUsage 等价。
//
// JSONStore 不维护增量计数（AddUsage 是空操作），因此"账面"与"现算"
// 本来就是同一个值，没有可漂移的余地。对账在这里恒等于空转，
// 但接口要齐——否则同一段对账代码在两种 Store 下要分叉。
func (s *JSONStore) ComputeUsage(ctx context.Context, namespace, accountID string) (*model.Usage, error) {
	return s.GetUsage(ctx, namespace, accountID)
}

// GetUsage 扫全量按 object key 去重累加。
//
// 口径与 model.Usage 一致：同一个对象被多条记录引用（去重策略
// new_record 下很常见）只算一次，已软删的记录仍然计入——
// 它的对象还在底层占着空间。
func (s *JSONStore) GetUsage(ctx context.Context, namespace, accountID string) (*model.Usage, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]struct{})
	usage := &model.Usage{Namespace: namespace, AccountID: accountID, UpdatedAt: time.Now()}
	for _, r := range s.resources {
		if r.Namespace != namespace || r.AccountID != accountID {
			continue
		}
		if _, ok := seen[r.ObjectKey]; ok {
			continue
		}
		seen[r.ObjectKey] = struct{}{}
		usage.Bytes += r.Size
		usage.Files++
	}
	return usage, nil
}

var (
	_ store.Store      = (*JSONStore)(nil)
	_ store.QuotaStore = (*JSONStore)(nil)
)
