package manager

import (
	"context"
	"errors"
	"io"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// VerifySignedObjectActive checks gateway-signed links against current resource availability.
func (m *DefaultManager) VerifySignedObjectActive(ctx context.Context, objectKey string) error {
	count, err := m.store.Count(ctx, &model.Query{Provider: m.provider.Name(), ObjectKey: objectKey, Status: model.ResourceStatusActive})
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrResourceNotVisible
	}
	return nil
}

// OpenObject 读取底层对象内容，供代理下载使用。
// 当前 provider 不支持服务端读取时返回 ErrProxyDownloadUnsupported，调用方应改用 302 跳转。
func (m *DefaultManager) OpenObject(ctx context.Context, objectKey string) (io.ReadCloser, *model.ObjectStat, error) {
	reader, ok := m.provider.(provider.ObjectReader)
	if !ok {
		return nil, nil, ErrProxyDownloadUnsupported
	}
	return reader.Open(ctx, objectKey)
}

// ErrProxyDownloadUnsupported 表示当前 provider 不支持服务端代理读流。
var ErrProxyDownloadUnsupported = errors.New("eresource: provider does not support proxy download")

// ErrRangeUnsupported 表示当前 provider 不支持按字节区间读取。
// 调用方应当退化成回吐整个对象（HTTP 允许服务端忽略 Range）。
var ErrRangeUnsupported = errors.New("eresource: provider does not support ranged reads")

// StatObject 读取底层对象的大小与类型，不拉取内容。
// HEAD 请求与 Range 请求的区间换算都依赖它。
func (m *DefaultManager) StatObject(ctx context.Context, objectKey string) (*model.ObjectStat, error) {
	return m.provider.Stat(ctx, objectKey)
}

// OpenObjectRange 按字节区间读取底层对象。
//
// offset 为起始字节（含），length <= 0 表示读到末尾。
// 返回的 ObjectStat.Size 是对象的完整大小，供调用方拼 Content-Range。
func (m *DefaultManager) OpenObjectRange(ctx context.Context, objectKey string, offset, length int64) (io.ReadCloser, *model.ObjectStat, error) {
	reader, ok := m.provider.(provider.RangeReader)
	if !ok {
		return nil, nil, ErrRangeUnsupported
	}
	return reader.OpenRange(ctx, objectKey, offset, length)
}
