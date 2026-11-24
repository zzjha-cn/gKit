package cdn

import (
	"context"
)

// CDNRefresher 由能够刷新 / 预热 CDN 的实现提供。
//
// 内置实现使用阿里云 CDN OpenAPI，调用方也可以注入其他实现。
//
// 什么时候需要它：访问策略从 public 改成 private、或者资源被删除之后，
// CDN 边缘可能还缓存着旧内容——不刷新的话，一次「收回公开」在缓存过期前
// 是不生效的。
type CDNRefresher interface {
	// Refresh 让这些 URL 在边缘立即失效。
	Refresh(ctx context.Context, urls []string) error
	// Preload 预热这些 URL 到边缘。
	Preload(ctx context.Context, urls []string) error
}
