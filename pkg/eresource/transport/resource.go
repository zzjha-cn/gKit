package transport

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

// queryStatusAll 列表接口显式要求返回全部状态（含已软删除）的取值。
const queryStatusAll = "all"

// Get 按 ID 返回资源元数据。
//
// 走的是和内容下载同一套租户 + 身份校验：元数据里带着 object key、
// 大小、上传人等信息，裸奔的元数据接口同样是越权读。
// 返回前统一过 View()，抹掉底层存储路径与 bucket。
func (h *HTTPHandler) Get(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	resource, err := h.manager.Get(r.Context(), r.PathValue(paramID), identity)
	if err != nil {
		h.fail(w, r, accessStatus(err), err)
		return
	}
	h.ok(w, resource.View())
}

// List 按条件分页查询资源，同时返回总数供后台分页展示。
func (h *HTTPHandler) List(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	query := h.listQuery(r, identity)
	resources, err := h.manager.List(r.Context(), query)
	if err != nil {
		h.fail(w, r, queryStatus(err), err)
		return
	}
	total, err := h.manager.Count(r.Context(), query)
	if err != nil {
		h.fail(w, r, queryStatus(err), err)
		return
	}
	items := make([]*model.Resource, 0, len(resources))
	for _, resource := range resources {
		items = append(items, resource.View())
	}
	h.ok(w, map[string]any{"total": total, "items": items, "offset": query.Offset, "limit": query.Limit})
}

// listQuery 从 query string 解析检索条件。
//
// 租户字段不取自 query string，而是由 ScopedQuery 用调用方身份强制覆盖：
// 列表接口没法「先查出来再判断」，隔离只能靠收窄检索条件实现，
// 而这个条件一旦允许客户端指定，换个 account_id 就能翻别人的资源。
func (h *HTTPHandler) listQuery(r *http.Request, identity *model.AccessRequest) *model.Query {
	query := &model.Query{
		IDs:           h.idList(r.URL.Query()["ids"]),
		Hash:          h.queryValue(r, "hash"),
		Provider:      h.queryValue(r, "provider"),
		BizType:       h.queryValue(r, "biz_type"),
		Status:        h.listStatus(r),
		AccessPolicy:  h.queryValue(r, "access_policy"),
		Creator:       h.queryValue(r, "creator"),
		Tags:          h.queryValues(r, "tags"),
		Metadata:      h.prefixedQuery(r, metadataFormPrefix),
		CreatedAfter:  parseTimeParam(h.queryValue(r, "created_after")),
		CreatedBefore: parseTimeParam(h.queryValue(r, "created_before")),
		Limit:         defaultListLimit,
	}
	if raw := h.queryValue(r, "offset"); raw != "" {
		if offset, err := strconv.ParseInt(raw, 10, 64); err == nil && offset > 0 {
			query.Offset = offset
		}
	}
	if raw := h.queryValue(r, "limit"); raw != "" {
		if limit, err := strconv.ParseInt(raw, 10, 64); err == nil && limit > 0 {
			query.Limit = min(limit, maxListLimit)
		}
	}
	return manager.ScopedQuery(query, identity)
}

// listStatus 解析列表的状态过滤条件。
//
// 不传时默认只看 active：开了软删除之后，「删掉的东西还在列表里」
// 对业务来说就是没删掉。要连已删除一起看，显式传 status=all。
func (h *HTTPHandler) listStatus(r *http.Request) string {
	switch status := h.queryValue(r, "status"); status {
	case "":
		return model.ResourceStatusActive
	case queryStatusAll:
		return ""
	default:
		return status
	}
}

// Delete 删除资源及底层对象。
func (h *HTTPHandler) Delete(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	if err := h.manager.Delete(r.Context(), r.PathValue(paramID), identity); err != nil {
		h.fail(w, r, accessStatus(err), err)
		return
	}
	h.ok(w, map[string]any{"deleted": true})
}

// Access 按访问策略返回最终可用地址（公开直链或时效签名链接）。
func (h *HTTPHandler) Access(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	url, err := h.manager.GetAccessURL(r.Context(), r.PathValue(paramID), identity)
	if err != nil {
		h.fail(w, r, accessStatus(err), err)
		return
	}
	h.ok(w, map[string]any{"url": url})
}

func (h *HTTPHandler) queryValue(r *http.Request, key string) string {
	return r.URL.Query().Get(key)
}

// queryValues 读取多值 query 参数，同时接受重复字段与逗号分隔两种写法。
func (h *HTTPHandler) queryValues(r *http.Request, key string) []string {
	return h.idList(r.URL.Query()[key])
}

func accessStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound),
		errors.Is(err, manager.ErrResourceInactive),
		// 跨租户一律按「不存在」回应：返回 403 等于确认该 ID 真实存在，
		// 攻击者可以据此枚举出别的租户有哪些资源。
		errors.Is(err, manager.ErrResourceNotVisible):
		return http.StatusNotFound
	case errors.Is(err, manager.ErrNotResourceOwner),
		errors.Is(err, manager.ErrHotlinkDenied):
		return http.StatusForbidden
	// private 资源缺身份是「请先登录」而不是「你不能看」，
	// 与签名路由的 ErrIdentityRequired 保持同一个状态码。
	case errors.Is(err, manager.ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, manager.ErrRateLimited):
		return http.StatusTooManyRequests
	default:
		// 只有已知的鉴权结论才会落到 4xx。兜底必须回退到 storeStatus：
		// 否则存储故障（如 mongo 选主超时）会被渲染成 403，
		// 监控上表现为一片「权限拒绝」——既不触发 5xx 告警，客户端也不会重试。
		return storeStatus(err)
	}
}
