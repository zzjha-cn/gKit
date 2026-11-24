// Package transport 提供 eresource 的 HTTP 协议接入层。
// 该层只负责协议解析与响应编码，所有业务编排都收敛在 manager 中。
package transport

import (
	"errors"
	"net/http"
	"strings"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// 路由与表单字段名
const (
	RouteGroup     = "/resource"
	FormFieldFile  = "file"
	paramID        = "id"
	paramObjectKey = "objectKey"
	// uploadFormMemory multipart 解析时留在内存里的上限。
	// 超出部分由 net/http 落到临时文件，进程退出或请求结束时清理。
	uploadFormMemory = 1 << 20
	defaultListLimit = 20
	maxListLimit     = 200
	// metadataFormPrefix 业务扩展字段的收参前缀：metadata.order_id=A1。
	// 用前缀而不是「一个 JSON 字符串字段」，是为了让上传表单、更新表单与
	// 列表查询串三处用同一种写法，不必为了传两个 K-V 引入一层编码。
	metadataFormPrefix = "metadata."
)

// Option 调整 HTTPHandler 的可选行为。
type Option func(*HTTPHandler)

// HTTPHandler 把 Manager 暴露为 HTTP 接口。
type HTTPHandler struct {
	manager        manager.Manager
	cfg            config.Config
	signedBasePath string
	// fetcher 是可选的远端抓取中转能力，cfg.Proxy.Enable 为 false 时为 nil，
	// 对应的路由也不会注册。
	fetcher *resourceFetcher
	// uploadSlots 是上传并发闸门，容量即允许的同时上传数。
	uploadSlots chan struct{}
	// resolveIdentity 还原调用方身份，默认取自网关注入的请求头，
	// 可用 WithIdentityResolver 换成接入方自己的鉴权结果。
	resolveIdentity IdentityResolver
	// directUploader 是可选的前端直传能力，未开启或 provider 不支持时为 nil，
	// 对应的路由也不会注册。
	directUploader manager.DirectUploader
	// multipartUploader 是可选的分片上传能力，未开启或 provider 不支持时为 nil。
	multipartUploader manager.MultipartUploader
}

// NewHTTPHandler 构造 HTTP 接入层。
// p 用于读取签名路由的挂载路径；传 nil 时使用 config.DefaultSignedBasePath。
//
// 启用了 cfg.Proxy.Enable 时，抓取中转会作为本 Handler 的附属能力一并装配，
// 其生命周期随 Handler，调用方需在退出前调用 Close 释放后台清理任务。
func NewHTTPHandler(mgr manager.Manager, p provider.Provider, cfg config.Config, opts ...Option) (*HTTPHandler, error) {
	if mgr == nil {
		return nil, errors.New("eresource: manager is nil")
	}
	signedBasePath := strings.Trim(cfg.Provider.SignedBasePath, "/")
	if signedBasePath == "" {
		if signed, ok := p.(interface{ SignedBasePath() string }); ok {
			signedBasePath = signed.SignedBasePath()
		} else {
			signedBasePath = config.DefaultSignedBasePath
		}
	}
	maxUploads := cfg.Manager.MaxConcurrentUploads
	if maxUploads <= 0 {
		maxUploads = config.DefaultMaxConcurrentUploads
	}
	h := &HTTPHandler{
		manager:        mgr,
		cfg:            cfg,
		signedBasePath: signedBasePath,
		uploadSlots:    make(chan struct{}, maxUploads),
		// 默认仍是请求头方案：不挂载解析器时行为与升级前完全一致。
		// 默认值不能是「不解析」——那会让模块的默认姿势退回零隔离。
		resolveIdentity: HeaderIdentityResolver(cfg),
	}
	for _, apply := range opts {
		apply(h)
	}
	if cfg.Manager.EnableDirectUpload {
		if uploader, ok := mgr.(manager.DirectUploader); ok {
			h.directUploader = uploader
		}
	}
	if cfg.Manager.EnableMultipartUpload {
		if uploader, ok := mgr.(manager.MultipartUploader); ok {
			// 再确认一次底层确实支持：配置打开但 provider 不具备这个能力时，
			// 注册出来的是四条永远 501 的路由，不如不注册。
			if _, supported := p.(provider.MultipartUploader); supported || p == nil {
				h.multipartUploader = uploader
			}
		}
	}
	if cfg.Proxy.Enable {
		fetcher, err := newResourceFetcher(cfg.Proxy, cfg.Manager.MaxFileSize)
		if err != nil {
			return nil, err
		}
		h.fetcher = fetcher
	}
	return h, nil
}

// Close 释放 Handler 持有的后台资源（目前是抓取中转的缓存清理任务）。
// 重复调用是安全的。
func (h *HTTPHandler) Close() {
	h.fetcher.Close()
}

// Register 注册全部路由。
//
// 注意：不要把 provider 的 RootDir 直接挂成静态目录，否则 protected/private
// 资源可以绕开验签被裸链下载。公开资源应单独放在只含 public 对象的目录下。
func (h *HTTPHandler) Register(mux *http.ServeMux) {
	// The signed subtree takes precedence over resource ID routes. Keep the ID
	// patterns in their own mux so the two sets of wildcards do not conflict.
	routes := http.NewServeMux()
	resourceID := RouteGroup + "/{" + paramID + "}"
	routes.HandleFunc("POST "+RouteGroup+"/upload", h.Upload)
	routes.HandleFunc("GET "+RouteGroup+"/list", h.List)
	// A methodless ID route keeps unknown POST endpoints at 404, including
	// /proxy-upload when proxying is disabled.
	routes.HandleFunc(resourceID, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			h.Get(w, r)
		case http.MethodPatch:
			h.Update(w, r)
		case http.MethodDelete:
			h.Delete(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	routes.HandleFunc("POST "+RouteGroup+"/batch/access", h.BatchAccess)
	routes.HandleFunc("POST "+RouteGroup+"/batch/delete", h.BatchDelete)
	routes.HandleFunc("GET "+resourceID+"/access", h.Access)
	// The GET patterns also dispatch HEAD to the same handler.
	routes.HandleFunc("GET "+resourceID+"/content", h.Content)
	if h.directUploader != nil {
		routes.HandleFunc("POST "+RouteGroup+"/upload/policy", h.DirectUploadPolicy)
		routes.HandleFunc("POST "+RouteGroup+"/upload/complete", h.DirectUploadComplete)
	}
	if h.multipartUploader != nil {
		routes.HandleFunc("POST "+RouteGroup+"/multipart/init", h.MultipartInit)
		routes.HandleFunc("POST "+RouteGroup+"/multipart/part", h.MultipartPart)
		routes.HandleFunc("POST "+RouteGroup+"/multipart/complete", h.MultipartComplete)
		routes.HandleFunc("POST "+RouteGroup+"/multipart/abort", h.MultipartAbort)
	}
	if h.fetcher != nil {
		routes.HandleFunc("POST "+RouteGroup+"/proxy-upload", h.ProxyUpload)
	}
	mux.HandleFunc(RouteGroup+"/", func(w http.ResponseWriter, r *http.Request) {
		// Middleware can pass a request copy whose multipart files are not
		// visible to the server's request cleanup.
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		routes.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /"+h.signedBasePath+"/{"+paramObjectKey+"...}", h.SignedContent)
}
