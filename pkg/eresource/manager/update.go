package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

var (
	// ErrEmptyUpdate 更新请求没带任何可改字段。
	ErrEmptyUpdate = errors.New("eresource: update request contains no mutable field")
	// ErrInvalidAccessPolicy 访问策略取值非法。
	ErrInvalidAccessPolicy = errors.New("eresource: access policy is invalid")
	// ErrInvalidResourceName 文件名非法。
	ErrInvalidResourceName = errors.New("eresource: resource name is invalid")
	// ErrObjectShared 底层对象被多条记录共享，无法单独调整它的访问策略。
	//
	// 开了去重（new_record 策略）之后，多条记录可能指向同一个对象。
	// 对象 ACL 是对象级的，改一条记录的访问策略会连带改掉所有兄弟记录的
	// 实际可见性——那是一次静默的越权，所以这里直接拒绝。
	ErrObjectShared = errors.New("eresource: object is shared by multiple resources, access policy cannot be changed")
)

// MaxResourceNameLength 文件名长度上限。
const MaxResourceNameLength = 255

// Update 更新资源的业务元数据。
//
// 不可改的是「这个对象是什么、属于谁」：存储布局、内容事实与身份归属。
// 可改的只有展示与分类字段，外加访问策略——后者是本接口唯一有安全后果的
// 改动，因此权限口径与删除一致（上传者本人 / 管理员 / 显式放开）。
//
// req 为 nil 表示可信的进程内调用，与 Get / Delete 同一套语义。
func (m *DefaultManager) Update(ctx context.Context, id string, update *model.UpdateRequest, req *model.AccessRequest) (*model.Resource, error) {
	if update.IsEmpty() {
		return nil, ErrEmptyUpdate
	}
	m.objectReferencesMu.Lock()
	defer m.objectReferencesMu.Unlock()
	resource, err := m.store.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = m.authorizeScope(resource, req); err != nil {
		return nil, err
	}
	// 已软删除的资源不可改：否则一次更新就能把它从回收站里捞回来，
	// 绕过删除权限本身。
	if err = m.authorizeIdentity(resource, req); err != nil {
		return nil, err
	}
	if err = m.authorizeMutate(resource, req); err != nil {
		return nil, err
	}

	patch := &model.ResourcePatch{}
	if err = m.patchDisplayFields(update, resource, patch); err != nil {
		return nil, err
	}
	if err = m.patchAccessPolicy(ctx, update, resource, patch); err != nil {
		return nil, err
	}
	if patch.IsEmpty() {
		// 传进来的值与现值完全一致：不必写库，也不该把 updated_at 推进。
		return m.fillURL(resource), nil
	}
	// 时间在这里定死而不是交给 Store：否则返回给调用方的那份副本
	// 带着零值 updated_at，与库里真正落下的时间对不上。
	patch.UpdatedAt = time.Now()
	if err = m.store.Patch(ctx, id, patch); err != nil {
		return nil, err
	}
	updated := resource.Clone()
	patch.Apply(updated)
	// 补丁里的 URL 永远是空串（见 patchAccessPolicy），对外那一份按当前配置重算。
	return m.fillURL(updated), nil
}

// patchDisplayFields 处理不涉及安全语义的展示与分类字段。
func (m *DefaultManager) patchDisplayFields(update *model.UpdateRequest, resource *model.Resource, patch *model.ResourcePatch) error {
	if update.Name != nil {
		name, err := validateResourceName(*update.Name)
		if err != nil {
			return err
		}
		if name != resource.Name {
			patch.Name = &name
		}
	}
	if update.BizType != nil {
		bizType := strings.TrimSpace(*update.BizType)
		// 复用上传侧那套严格校验：biz_type 在上传时是路径段，
		// 两边允许的字符集不一致会让同一个值在列表里查得到、重传时被拒。
		if err := provider.ValidateIdentitySegments(&model.UploadRequest{BizType: bizType}); err != nil {
			return err
		}
		if bizType != resource.BizType {
			patch.BizType = &bizType
		}
	}
	if update.Tags != nil {
		tags := normalizeTags(update.Tags)
		if !sameStrings(tags, resource.Tags) {
			patch.Tags = tags
		}
	}
	if update.Metadata != nil {
		merged, changed, err := mergeMetadata(resource.Metadata, update.Metadata)
		if err != nil {
			return err
		}
		if changed {
			patch.Metadata = merged
		}
	}
	return nil
}

// patchAccessPolicy 处理访问策略变更，必要时连带调整底层对象 ACL 与公开直链。
func (m *DefaultManager) patchAccessPolicy(ctx context.Context, update *model.UpdateRequest, resource *model.Resource, patch *model.ResourcePatch) error {
	if update.AccessPolicy == nil {
		return nil
	}
	policy := strings.TrimSpace(*update.AccessPolicy)
	if !validAccessPolicy(policy) {
		return fmt.Errorf("%w: %q", ErrInvalidAccessPolicy, policy)
	}
	if policy == resource.AccessPolicy {
		return nil
	}
	// 转成 public 要重新过一遍危险类型拦截。上传时这条资源可能是以
	// private 身份进来的，当时根本没走这道校验——少了这一步，
	// 「先传成 private，再改成 public」就是 T3 的完整绕过路径。
	//
	// 这里拿不到文件内容，只能凭声明类型与扩展名判断：真要做内容嗅探
	// 就得把对象整个拉回来，代价与收益不成比例（上传时已经嗅探过一次）。
	if err := m.validatePublicContent(policy, resource.MimeType, resource.Ext, nil); err != nil {
		return err
	}
	shared, err := m.objectShared(ctx, resource)
	if err != nil {
		return err
	}
	if shared {
		return fmt.Errorf("%w: object_key=%s", ErrObjectShared, resource.ObjectKey)
	}
	// 先改对象 ACL 再改元数据，两个方向都是「失败时偏向更严」：
	//   - 转 public 时 ACL 先成功、元数据失败 → 对象可读但没有任何地方发布过它的
	//     直链（object key 带哈希与随机后缀，猜不出来），没有实际暴露；
	//   - 转 private 时 ACL 先成功、元数据失败 → 元数据还说 public，发出去的
	//     直链开始 404，是一次可见的功能故障而不是一次静默的泄露。
	// 反过来先写元数据，转 private 的失败窗口里对象仍然人人可读。
	if err = m.applyObjectACL(ctx, resource.ObjectKey, policy); err != nil {
		return err
	}
	patch.AccessPolicy = &policy
	// URL 一律写回空串：直链不再落库（B13），读取时按当时的配置重算。
	// 这里顺手把老记录里的域名快照清掉，省得库里留着一份谁也不读的陈值。
	empty := ""
	patch.URL = &empty
	return nil
}

// applyObjectACL 把访问策略同步到底层对象。
// Provider 不具备对象级 ACL（如本地盘）时是空操作。
func (m *DefaultManager) applyObjectACL(ctx context.Context, objectKey, accessPolicy string) error {
	setter, ok := m.provider.(provider.ACLSetter)
	if !ok {
		return nil
	}
	return setter.SetObjectACL(ctx, objectKey, accessPolicy)
}

// objectShared 判断底层对象是否还被别的记录引用。
//
// 这里有意把已软删除的记录也算进来：它们随时可能被恢复，
// 而它们指向的仍然是同一个对象。
func (m *DefaultManager) objectShared(ctx context.Context, resource *model.Resource) (bool, error) {
	count, err := m.objectReferences(ctx, resource)
	if err != nil {
		return false, err
	}
	return count > 1, nil
}

// objectReferences 返回指向同一个底层对象的记录总数（含自己）。
func (m *DefaultManager) objectReferences(ctx context.Context, resource *model.Resource) (int64, error) {
	if resource.ObjectKey == "" {
		return 0, nil
	}
	return m.store.Count(ctx, &model.Query{
		Provider:  resource.Provider,
		ObjectKey: resource.ObjectKey,
	})
}

// validAccessPolicy 判断访问策略取值是否合法。
func validAccessPolicy(policy string) bool {
	switch policy {
	case model.AccessPolicyPublic, model.AccessPolicyProtected, model.AccessPolicyPrivate:
		return true
	}
	return false
}

// validateResourceName 校验展示文件名。
//
// 它会被写进下载响应的 Content-Disposition，因此必须挡掉控制字符——
// 一个换行就能往响应头里再插一行。路径分隔符同样拒绝：文件名不是路径。
func validateResourceName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidResourceName)
	}
	if len(name) > MaxResourceNameLength {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidResourceName, MaxResourceNameLength)
	}
	if strings.ContainsAny(name, "/\\\"") {
		return "", fmt.Errorf("%w: must not contain path separators or quotes", ErrInvalidResourceName)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: contains control characters", ErrInvalidResourceName)
		}
	}
	return name, nil
}

// normalizeTags 去掉空白标签并去重，保持调用方给出的顺序。
func normalizeTags(tags []string) []string {
	normalized := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		normalized = append(normalized, tag)
	}
	return normalized
}

// mergeMetadata 按「空值即删除」的语义合并 metadata，返回合并结果与是否真的有改动。
//
// 合并而不是整体替换：调用方手里的那份 map 可能是几秒钟前读出来的，
// 整体写回会把这期间别人写入的 key 一起抹掉，而它自己完全不知情。
func mergeMetadata(current, incoming map[string]string) (map[string]string, bool, error) {
	merged := make(map[string]string, len(current)+len(incoming))
	for k, v := range current {
		merged[k] = v
	}
	changed := false
	for key, value := range incoming {
		if err := model.ValidateMetadataKey(key); err != nil {
			return nil, false, err
		}
		if value == "" {
			if _, exists := merged[key]; exists {
				delete(merged, key)
				changed = true
			}
			continue
		}
		if merged[key] != value {
			merged[key] = value
			changed = true
		}
	}
	return merged, changed, nil
}

// sameStrings 比较两个字符串切片是否逐项相同。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
