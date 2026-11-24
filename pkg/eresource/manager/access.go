package manager

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// Get 读取资源元数据。
//
// req 承载调用方身份，会用于租户与访问策略校验；传 nil 表示可信的进程内调用，
// 此时跳过租户校验（HTTP 接入层永远传非 nil）。
func (m *DefaultManager) Get(ctx context.Context, id string, req *model.AccessRequest) (*model.Resource, error) {
	resource, err := m.store.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = m.authorizeScope(resource, req); err != nil {
		return nil, err
	}
	if err = m.authorizeIdentity(resource, req); err != nil {
		return nil, err
	}
	return m.fillURL(resource), nil
}

func (m *DefaultManager) GetAccessURL(ctx context.Context, id string, req *model.AccessRequest) (string, error) {
	resource, err := m.store.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	if err = m.verifyResourceAccess(ctx, resource, req); err != nil {
		return "", err
	}
	userID := ""
	if req != nil {
		userID = req.UserID
	}
	return m.accessURLFor(ctx, resource, userID)
}

// GetVerified 一次查库完成「取元数据 + 完整访问校验」。
//
// 等价于 Get + VerifyAccess，但只做一次 GetByID。受控下载路径上这是每次
// 下载都要付的成本：两次 GetByID 之间元数据还可能变，拿到的是一份
// 可能与校验对象不一致的元数据——合并之后这个窗口也一并消失了。
func (m *DefaultManager) GetVerified(ctx context.Context, id string, req *model.AccessRequest) (*model.Resource, error) {
	resource, err := m.store.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = m.verifyResourceAccess(ctx, resource, req); err != nil {
		return nil, err
	}
	return m.fillURL(resource), nil
}

func (m *DefaultManager) VerifyAccess(ctx context.Context, id string, req *model.AccessRequest) error {
	resource, err := m.store.GetByID(ctx, id)
	if err != nil {
		return err
	}
	return m.verifyResourceAccess(ctx, resource, req)
}

// authorizeScope 校验调用方与资源处于同一租户。
//
// 这是读取侧唯一的隔离边界：object key 的租户前缀只影响物理布局，
// 资源 ID 本身是全局的，任何人拿到 ID 都能命中 store.GetByID。
// 少了这道校验，写入侧那套 namespace / account_id 隔离在读取时完全穿透。
//
// 刻意做成全等比较而不是「空则不校验」：调用方没传租户信息时，
// 它只能访问同样没有租户信息的资源。一旦允许空值通配，
// 不传参数反而成了权限最大的调用方式。
//
// req 为 nil 表示可信的进程内调用（如后台任务），跳过校验。
// nil 与「字段为空的 req」是两种不同的语义，必须在调用点显式写出来。
func (m *DefaultManager) authorizeScope(resource *model.Resource, req *model.AccessRequest) error {
	if req == nil {
		return nil
	}
	// 平台级共享资源跨租户只读可见（B10：默认头像、模板素材这类东西，
	// 否则只能每个租户复制一份）。放行的只有读——authorizeMutate
	// 不看这个字段，共享资源的修改与删除仍然锁在归属租户手里。
	// 这一档刻意放在全等比较之前，且刻意只做在 authorizeScope 里：
	// 挪到 authorizeMutate 就是一个跨租户写入漏洞。
	if resource.Shared {
		return nil
	}
	if resource.Namespace != req.Namespace || resource.AccountID != req.AccountID {
		// 刻意复用「不存在」语义：返回 403 等于告诉调用方这个 ID 真实存在，
		// 把越权探测降级成了资源存在性探测。
		return ErrResourceNotVisible
	}
	return nil
}

// authorizeIdentity 校验资源状态与调用方身份，不含防盗链与频控。
//
// 与 authorizeScope 一致：req 为 nil 表示可信的进程内调用，跳过身份校验。
// 防盗链与频控本来就只在 req != nil 时才生效，这里保持同一套语义，
// 免得「nil 代表什么」在同一个文件里有两种解释。
// 资源状态例外——非 active 的资源对谁都不该可见，与调用方是谁无关。
func (m *DefaultManager) authorizeIdentity(resource *model.Resource, req *model.AccessRequest) error {
	if resource.Status != model.ResourceStatusActive {
		return ErrResourceInactive
	}
	if req == nil {
		return nil
	}
	if resource.AccessPolicy == model.AccessPolicyPrivate && req.UserID == "" {
		return ErrUnauthenticated
	}
	return nil
}

// authorizeMutate 校验改动权限，删除与更新共用。
//
// 两者合用一套口径，是因为「把一份 private 素材改成 public」与删除同级危险：
// 都是不经原上传者同意就改变了这份内容的命运。
//
// 删除会连底层对象一起抹掉，不可撤销，因此默认只允许上传者本人执行。
// 这里同样是全等比较：没有 Creator 的资源只能由同样没有身份的调用方删除。
// 后台管理类场景通过 access.allow_cross_creator_delete 放开，
// 放开之后仍然受 authorizeScope 的租户边界约束。
func (m *DefaultManager) authorizeMutate(resource *model.Resource, req *model.AccessRequest) error {
	if req == nil {
		return nil
	}
	if resource.Namespace != req.Namespace || resource.AccountID != req.AccountID {
		return ErrResourceNotVisible
	}
	if m.config.Access.AllowCrossCreatorDelete {
		return nil
	}
	// 管理员一档：上传者本人之外，本租户的管理员也可以代删。
	// 少了这一档，allow_cross_creator_delete 就是个全局 all-or-nothing 开关——
	// 关着则员工离职、运营代删、批量清理全部卡死；开着则同租户内
	// 任意一个 C 端用户都能删别人的文件。
	if req.IsAdmin && adminCoversBizType(req.AdminBizTypes, resource.BizType) {
		return nil
	}
	if req.UserID != resource.Creator {
		return ErrNotResourceOwner
	}
	return nil
}

// adminCoversBizType 判断管理员权限是否覆盖该资源所属的业务线。
//
// allowed 为空表示不限定（IsAdmin 原有的全租户语义）。非空时做全等匹配，
// 不做前缀或通配：授权范围靠模糊匹配表达，迟早会出现
// 「本想授权 contract_draft，结果连 contract 都能删」这种事。
func adminCoversBizType(allowed []string, bizType string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if candidate == bizType {
			return true
		}
	}
	return false
}

func (m *DefaultManager) verifyResourceAccess(ctx context.Context, resource *model.Resource, req *model.AccessRequest) error {
	// 0. 租户隔离边界，先于其它一切校验
	if err := m.authorizeScope(resource, req); err != nil {
		return err
	}

	// 1. 资源状态与基础鉴权控制
	if err := m.authorizeIdentity(resource, req); err != nil {
		return err
	}

	// 2. Referer 防盗链校验
	if resource.HotlinkProtection && m.config.Access.EnableHotlinkProtection && req != nil {
		switch {
		case req.Referer == "":
			// 浏览器在部分场景确实不带 Referer，是否放行由配置决定
			if !m.config.Access.AllowEmptyReferer {
				return fmt.Errorf("%w: empty referer", ErrHotlinkDenied)
			}
		case !m.isRefererAllowed(req.Referer):
			return fmt.Errorf("%w: referer not allowed", ErrHotlinkDenied)
		}
	}

	// 3. 基础限流防刷校验（基于内存或简单规则，可由外部缓存如 Redis 提供更高级限流能力）
	if req != nil {
		if err := m.checkRateLimit(ctx, resource, req); err != nil {
			return err
		}
	}

	return nil
}

// isRefererAllowed 检查请求 Referer 的主机名是否命中白名单。
//
// 必须解析出 Host 再比对：用子串匹配的话，
// https://evil.com/?x=example.com 这种 URL 会被误判为来自 example.com。
func (m *DefaultManager) isRefererAllowed(referer string) bool {
	whitelist := m.config.Access.RefererWhitelist
	if len(whitelist) == 0 {
		return true // 未配置白名单时不做限制
	}
	parsed, err := url.Parse(referer)
	if err != nil || parsed.Host == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, entry := range whitelist {
		pattern := strings.ToLower(strings.TrimSpace(entry))
		if pattern == "" {
			continue
		}
		// "*.example.com" 同时匹配 example.com 与任意层级子域名
		if root, ok := strings.CutPrefix(pattern, "*."); ok {
			if host == root || strings.HasSuffix(host, "."+root) {
				return true
			}
			continue
		}
		if host == pattern {
			return true
		}
	}
	return false
}
