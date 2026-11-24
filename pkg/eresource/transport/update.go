package transport

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// Update 更新资源的业务元数据。
//
// 请求体只需要带上要改的字段：没出现的字段保持原值。
// 这与「整条写回」的区别不只是省几个参数——整条写回会让调用方把一份
// 可能已经过期的快照推回来，把这期间别人改的字段一起冲掉。
//
// 支持的字段：
//
//	name             展示文件名
//	biz_type         业务分类
//	access_policy    访问策略，会连带调整底层对象 ACL
//	tags             标签，整体替换；只传一个空值表示清空
//	metadata.<key>   业务扩展字段，合并语义，空值表示删除该 key
func (h *HTTPHandler) Update(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	update := &model.UpdateRequest{
		Name:         h.optionalForm(r, "name"),
		BizType:      h.optionalForm(r, "biz_type"),
		AccessPolicy: h.optionalForm(r, "access_policy"),
		Tags:         h.optionalFormArray(r, "tags"),
		Metadata:     h.prefixedForm(r, metadataFormPrefix),
	}
	resource, err := h.manager.Update(r.Context(), r.PathValue(paramID), update, identity)
	if err != nil {
		h.fail(w, r, updateStatus(err), err)
		return
	}
	h.ok(w, resource.View())
}

// BatchAccess 批量签发访问地址。
//
// 单条失败不影响整批：结果分成 items 与 failed 两段，
// 调用方能明确知道哪些 ID 拿到了地址、哪些没有以及为什么。
// 整批一起失败（400 / 401）只发生在请求本身不合法时。
func (h *HTTPHandler) BatchAccess(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	results, err := h.manager.BatchAccessURL(r.Context(), h.batchIDs(r), identity)
	if err != nil {
		h.fail(w, r, batchStatus(err), err)
		return
	}
	h.okBatch(w, results, "url")
}

// BatchDelete 批量删除资源。
func (h *HTTPHandler) BatchDelete(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r)
	if err != nil {
		h.fail(w, r, identityStatus(err), err)
		return
	}
	// 删除会触碰底层对象，与单条删除共用上传闸门之外的预算：
	// 这里切断请求超时，理由与上传一致——删到一半被取消会留下
	// 一半已删一半没删的中间状态，而调用方看到的只是一次超时。
	ctx, cancel := h.uploadContext(r)
	defer cancel()

	results, err := h.manager.BatchDelete(ctx, h.batchIDs(r), identity)
	if err != nil {
		h.fail(w, r, batchStatus(err), err)
		return
	}
	h.okBatch(w, results, "deleted")
}

// okBatch 渲染批量结果：成功项进 items，失败项带上各自的状态码进 failed。
func (h *HTTPHandler) okBatch(w http.ResponseWriter, results []manager.BatchResult, successField string) {
	items := make([]map[string]any, 0, len(results))
	failed := make([]map[string]any, 0)
	for _, result := range results {
		if result.Err != nil {
			failed = append(failed, map[string]any{
				"id":      result.ID,
				"code":    accessStatus(result.Err),
				"message": result.Err.Error(),
			})
			continue
		}
		item := map[string]any{"id": result.ID}
		if successField == "url" {
			item["url"] = result.URL
		} else {
			item[successField] = true
		}
		items = append(items, item)
	}
	h.ok(w, map[string]any{"items": items, "failed": failed})
}

// batchIDs 解析批量接口的 ID 列表。
//
// 同时接受重复字段（ids=a&ids=b）与逗号分隔（ids=a,b）：
// 前者是表单的标准写法，后者是大多数前端拼 URL 时的下意识写法，
// 只支持其中一种的结果是另一种悄悄退化成「一个很长的 ID」然后全部查不到。
func (h *HTTPHandler) batchIDs(r *http.Request) []string {
	ids := h.idList(postFormValues(r, "ids"))
	if len(ids) == 0 {
		ids = h.idList(r.URL.Query()["ids"])
	}
	return ids
}

// idList 把原始取值展开成 ID 列表，逗号分隔的值会被拆开。
func (h *HTTPHandler) idList(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	ids := make([]string, 0, len(raw))
	for _, value := range raw {
		for _, id := range strings.Split(value, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// optionalForm 读取一个「可选」表单字段：字段不存在时返回 nil。
//
// 必须区分「没传」与「传了空串」：前者是不改，后者是清空，
// 用空字符串统一表示的话，调用方永远没办法把一个字段改回空。
func (h *HTTPHandler) optionalForm(r *http.Request, key string) *string {
	values := postFormValues(r, key)
	if len(values) == 0 {
		return nil
	}
	return &values[0]
}

// optionalFormArray 读取一个「可选」的多值表单字段。
// 字段整个不出现时返回 nil（不改）；出现但值全为空时返回空切片（清空）。
func (h *HTTPHandler) optionalFormArray(r *http.Request, key string) []string {
	raw := postFormValues(r, key)
	if len(raw) == 0 {
		return nil
	}
	values := make([]string, 0, len(raw))
	for _, value := range raw {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

// prefixedForm 收集表单里带指定前缀的字段，返回去掉前缀后的 K-V。
func (h *HTTPHandler) prefixedForm(r *http.Request, prefix string) map[string]string {
	_ = r.ParseMultipartForm(uploadFormMemory)
	return collectPrefixed(r.PostForm, prefix)
}

// prefixedQuery 收集 query string 里带指定前缀的参数。
func (h *HTTPHandler) prefixedQuery(r *http.Request, prefix string) map[string]string {
	if r.URL == nil {
		return nil
	}
	return collectPrefixed(r.URL.Query(), prefix)
}

// collectPrefixed 从一组取值里挑出带前缀的项，多值时取第一个。
func collectPrefixed(values map[string][]string, prefix string) map[string]string {
	var collected map[string]string
	for key, list := range values {
		if !strings.HasPrefix(key, prefix) || len(list) == 0 {
			continue
		}
		name := strings.TrimPrefix(key, prefix)
		if name == "" {
			continue
		}
		if collected == nil {
			collected = make(map[string]string, 4)
		}
		collected[name] = list[0]
	}
	return collected
}

// parseTimeParam 解析时间参数，同时接受 RFC3339 与 Unix 秒。
// 解析不了就当作没传：时间范围是收窄条件，一个写错的值不该让整个列表请求失败。
func parseTimeParam(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0)
	}
	return time.Time{}
}

// queryStatus 区分「检索条件写错了」与「存储出问题了」。
func queryStatus(err error) int {
	if errors.Is(err, model.ErrInvalidQuery) {
		return http.StatusBadRequest
	}
	return storeStatus(err)
}

// batchStatus 映射批量接口的整批失败。
func batchStatus(err error) int {
	switch {
	case errors.Is(err, manager.ErrEmptyBatch),
		errors.Is(err, manager.ErrBatchTooLarge):
		return http.StatusBadRequest
	default:
		return queryStatus(err)
	}
}

// updateStatus 映射更新接口的错误。
func updateStatus(err error) int {
	switch {
	case errors.Is(err, manager.ErrEmptyUpdate),
		errors.Is(err, manager.ErrInvalidAccessPolicy),
		errors.Is(err, manager.ErrInvalidResourceName),
		errors.Is(err, model.ErrInvalidQuery),
		errors.Is(err, provider.ErrInvalidSegment):
		return http.StatusBadRequest
	// 对象被多条记录共享，改一条的访问策略会殃及其它记录：
	// 这是资源当前状态与请求冲突，不是参数写错，所以是 409。
	case errors.Is(err, manager.ErrObjectShared):
		return http.StatusConflict
	case errors.Is(err, manager.ErrUnsafePublicContent):
		return http.StatusUnsupportedMediaType
	default:
		return accessStatus(err)
	}
}
