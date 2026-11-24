package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// exportPageSize 导出与清退的分页大小。
// 不做成配置：这两个操作都是运维动作，没人需要调它，
// 而暴露出去只会多一个可以被设成 100000 的旋钮。
const exportPageSize = 200

var ErrTenantIdentityRequired = errors.New("eresource: namespace and account ID are required")

// ExportTenant 分页回调式导出某个租户的全部资源元数据。
//
// 做成回调而不是一次性返回切片：导出的典型场景是客户离场或合规取证，
// 数据量可以很大，全量装进内存既没必要也不安全。
// fn 返回错误会立刻中止导出并把该错误透出去。
//
// 导出的是脱敏后的视图（View），底层存储布局不随数据一起交出去。
func (m *DefaultManager) ExportTenant(ctx context.Context, namespace, accountID string,
	fn func(resources []*model.Resource) error) error {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(accountID) == "" {
		return ErrTenantIdentityRequired
	}
	if fn == nil {
		return errors.New("eresource: export callback is nil")
	}
	var offset int64
	for {
		page, err := m.store.List(ctx, &model.Query{
			Namespace: namespace,
			AccountID: accountID,
			Offset:    offset,
			Limit:     exportPageSize,
		})
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		view := make([]*model.Resource, 0, len(page))
		for _, resource := range page {
			view = append(view, m.fillURL(resource).View())
		}
		if err = fn(view); err != nil {
			return err
		}
		if int64(len(page)) < exportPageSize {
			return nil
		}
		offset += int64(len(page))
	}
}

// ErrPurgeNotConfirmed 清退未确认。
var ErrPurgeNotConfirmed = errors.New("eresource: tenant purge requires an explicit confirmation token")

// PurgeTenant 彻底删除某个租户的全部资源，包括底层对象，不可撤销。
//
// confirm 必须等于 "<namespace>/<accountID>"。这不是防御攻击——能调到这个
// 方法的代码本来就有完全权限——而是防止一次参数写反把别的租户抹掉。
// 一个只能靠「调用方传对了参数」来保证的不可逆操作，值得多一道确认。
//
// 与 Delete 不同，这里**不走软删除**：清退的语义就是数据必须真的消失
// （GDPR 的删除权、客户离场的数据销毁证明），留一个回收窗口反而不合规。
//
// 单条失败不中断整批，返回已清理条数与合并后的错误：
// 一个删不掉的对象不该把后面几万条卡住。
func (m *DefaultManager) PurgeTenant(ctx context.Context, namespace, accountID, confirm string) (int, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(accountID) == "" {
		return 0, ErrTenantIdentityRequired
	}
	if confirm != namespace+"/"+accountID {
		return 0, fmt.Errorf("%w: expected %q", ErrPurgeNotConfirmed, namespace+"/"+accountID)
	}
	purged := 0
	var errs []error
	for {
		page, err := m.store.List(ctx, &model.Query{
			Namespace: namespace,
			AccountID: accountID,
			Limit:     exportPageSize,
		})
		if err != nil {
			return purged, errors.Join(append(errs, err)...)
		}
		if len(page) == 0 {
			return purged, errors.Join(errs...)
		}
		progressed := false
		for _, resource := range page {
			if resource.Namespace != namespace || resource.AccountID != accountID {
				return purged, errors.Join(append(errs, ErrResourceNotVisible)...)
			}
			if err = m.purge(ctx, resource); err != nil {
				errs = append(errs, fmt.Errorf("purge %s: %w", resource.ID, err))
				continue
			}
			purged++
			progressed = true
		}
		// 整页都删不掉就停下，否则 List 每次拿回同一页，循环永远不结束。
		if !progressed {
			return purged, errors.Join(errs...)
		}
	}
}
