package manager

import (
	"context"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

func (m *DefaultManager) List(ctx context.Context, query *model.Query) ([]*model.Resource, error) {
	// 检索条件的合法性在这里把关而不是交给 Store：metadata 的 key 会被
	// 拼进存储层的字段路径，两个 Store 各校验一遍迟早会走岔。
	if err := query.Validate(); err != nil {
		return nil, err
	}
	resources, err := m.store.List(ctx, query)
	if err != nil {
		return nil, err
	}
	for _, resource := range resources {
		m.fillURL(resource)
	}
	return resources, nil
}

// ScopedQuery 把调用方身份写进检索条件，覆盖 query 里原有的租户字段。
//
// 列表接口无法像 Get 那样「先查出来再判断」，隔离只能靠强制收窄检索条件。
// 必须是覆盖而不是「为空时填充」：否则客户端自己传一个 account_id
// 就能把范围改到别的租户上。
func ScopedQuery(query *model.Query, req *model.AccessRequest) *model.Query {
	if query == nil {
		query = &model.Query{}
	}
	if req == nil {
		return query
	}
	query.Namespace = req.Namespace
	query.AccountID = req.AccountID
	return query
}

// Count 返回匹配条件的资源总数，忽略分页参数，供后台分页展示总量。
func (m *DefaultManager) Count(ctx context.Context, query *model.Query) (int64, error) {
	if err := query.Validate(); err != nil {
		return 0, err
	}
	return m.store.Count(ctx, query)
}

// validateMetadataMap 校验 metadata 的 key 是否可以安全落库与检索。
//
// 写入侧必须和检索侧用同一把尺子：一个带着 "$" 或 "." 的 key 即便永远查不出来，
// 也已经让存储层的文档结构脱离预期（Mongo 会把 "a.b" 解释成嵌套字段）。
func validateMetadataMap(metadata map[string]string) error {
	for key := range metadata {
		if err := model.ValidateMetadataKey(key); err != nil {
			return err
		}
	}
	return nil
}
