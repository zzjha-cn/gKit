package transport

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// 身份请求头。必须由网关 / 鉴权中间件按会话覆写，不能透传客户端原值。
const (
	HeaderNamespace = "X-Resource-Namespace"
	HeaderAccountID = "X-Resource-Account-Id"
	// HeaderClientIP 调用方真实 IP，用于频控计数。
	// 与租户头同一套信任模型：由网关按连接来源覆写，不接受客户端自带值。
	HeaderClientIP = "X-Resource-Client-Ip"
	// HeaderUserID 调用方用户标识，用于归属校验、private 鉴权与签名地址的用户绑定。
	HeaderUserID = "X-User-Id"
	// HeaderIdempotencyKey 幂等键。同一租户下重复提交同一个键时，
	// 第二次及以后直接返回首次的结果，不会产生第二条记录与第二个对象。
	// 走请求头而不是表单字段：重试的是整个 HTTP 请求，
	// 键放在头上，客户端的重试逻辑不必重新组装 multipart 体。
	HeaderIdempotencyKey = "Idempotency-Key"
	// HeaderIsAdmin 调用方是否为本租户管理员，取值 1 / true / yes。
	// 仅在 access.trust_admin_header 打开时生效，同样必须由网关覆写。
	HeaderIsAdmin = "X-Resource-Is-Admin"
)

// HeaderIdentityResolver 返回默认的请求头身份解析器。
//
// 租户字段只认请求头，不认 query / form：这些头必须由网关或鉴权中间件
// 依据会话覆写，绝不能让客户端自带。如果直接信任客户端传的 account_id，
// 下面所有的租户校验都只是在跟攻击者自己填的值做比较。
//
// 选请求头而不是请求参数，原因不是「头更安全」——两者客户端都能伪造——
// 而是只有请求头能被反向代理无条件覆写：nginx 一条 proxy_set_header
// 就能对整个路由前缀生效，而表单字段藏在 multipart body 里，
// 网关要改它就得解析重组整个请求体（包括那个几十 MB 的文件 part）。
//
// 服务自己有鉴权中间件时，用 WithIdentityResolver 换掉它更直接：
// 会话信息本来就在 Context 里，不必绕一圈塞进头再读出来。
func HeaderIdentityResolver(cfg config.Config) IdentityResolver {
	return func(r *http.Request) (*model.AccessRequest, error) {
		return &model.AccessRequest{
			Namespace: r.Header.Get(HeaderNamespace),
			AccountID: r.Header.Get(HeaderAccountID),
			UserID:    r.Header.Get(HeaderUserID),
			IsAdmin:   trustedAdminHeader(cfg, r),
		}, nil
	}
}

// trustedAdminHeader 读取管理员标记头。
// access.trust_admin_header 关闭时一律返回 false：网关没有无条件覆写该头的话，
// 信任它等于让任何调用方自带一行请求头就拿到全租户的删除权。
func trustedAdminHeader(cfg config.Config, r *http.Request) bool {
	if !cfg.Access.TrustAdminHeader {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get(HeaderIsAdmin))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// identity 还原调用方身份，补齐风控字段，并做 fail-closed 校验。
//
// 所有需要租户边界的路由都必须经由这里取身份，不能直接调解析器：
// 漏掉一处就等于在那条路由上留了一个不设防的入口。
//
// require_identity 的校验刻意放在解析之后，对任何来源一视同仁：
// 它检的是「解析结果里租户是不是空的」，与身份来自请求头还是会话无关。
func (h *HTTPHandler) identity(r *http.Request) (*model.AccessRequest, error) {
	req, err := h.resolveIdentity(r)
	if err != nil {
		return nil, err
	}
	if req == nil {
		// 等价于「匿名调用方」：它只能访问同样没有租户信息的资源，
		// 而不是放行一切——全等比较的语义见 manager.authorizeScope。
		req = &model.AccessRequest{}
	}
	// 风控与审计字段由本层统一补齐：它们取自连接本身而不是业务身份，
	// 解析器显式填了就尊重它（例如内部 RPC 透传的真实来源）。
	if req.ClientIP == "" {
		req.ClientIP = h.clientIP(r)
	}
	if req.UserAgent == "" {
		req.UserAgent = r.Header.Get("User-Agent")
	}
	if req.Referer == "" {
		req.Referer = r.Header.Get("Referer")
	}
	if h.cfg.Access.RequireIdentity && (req.Namespace == "" || req.AccountID == "") {
		return nil, fmt.Errorf("%w: namespace / account_id", ErrIdentityMissing)
	}
	return req, nil
}

// clientIP 解析调用方 IP，用于频控计数。
//
// 使用受信网关注入的 HeaderClientIP，字段缺失或格式非法时使用 TCP 对端地址。
func (h *HTTPHandler) clientIP(r *http.Request) string {
	if raw := strings.TrimSpace(r.Header.Get(HeaderClientIP)); raw != "" {
		if net.ParseIP(raw) != nil {
			return raw
		}
	}
	host, _, _ := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	return host
}

// applyUploadIdentity 用解析出的可信身份覆盖上传请求的租户与创建者字段。
//
// 写入侧必须和读取侧用同一个身份来源。取自表单的话，调用方只要改个
// account_id 就能把文件写进别的租户、或者冒充别人当 creator；
// 开了去重时更糟——指定目标租户上传一份已知内容，命中去重后返回的是
// 那条记录的完整元数据，等于一个跨租户的存在性探测接口。
//
// 表单仍然带了这些字段时：值与可信身份一致则放行，不一致直接 400。
// 可信身份为空而表单有值同样算冲突，这正好覆盖「网关没注入头、客户端还在
// 传表单」这个最危险的组合。
func (h *HTTPHandler) applyUploadIdentity(r *http.Request, req *model.UploadRequest) error {
	trusted, err := h.identity(r)
	if err != nil {
		return err
	}
	fields := []struct {
		name   string
		form   string
		target *string
		value  string
	}{
		{"namespace", h.formValue(r, "namespace"), &req.Namespace, trusted.Namespace},
		{"account_id", h.formValue(r, "account_id"), &req.AccountID, trusted.AccountID},
		{"creator", h.formValue(r, "creator"), &req.Creator, trusted.UserID},
	}
	for _, f := range fields {
		if f.form != "" && f.form != f.value {
			return fmt.Errorf("%w: %s", ErrIdentityConflict, f.name)
		}
		*f.target = f.value
	}
	// 管理员标记同样只认可信来源：它决定 shared 字段会不会被采纳。
	req.IsAdmin = trusted.IsAdmin
	return nil
}

// identityStatus 区分「身份拿不到」与「客户端自己传错了」。
//
// 只有表单字段与可信身份冲突算 400（客户端的参数问题），
// 其余——身份头缺失、自定义解析器判定不可信——一律 401：
// 前者是服务端配置问题，混在 400 里会被当成参数错误而被忽略。
func identityStatus(err error) int {
	if errors.Is(err, ErrIdentityConflict) {
		return http.StatusBadRequest
	}
	return http.StatusUnauthorized
}

// ErrIdentityConflict 表单里的身份字段与解析出的可信身份不一致。
//
// 刻意不静默忽略表单值：还在用表单传租户的老客户端必须立刻收到报错，
// 否则它会以为自己写进了 A 租户，实际落在了可信身份指定的 B 租户里——
// 这种静默改写比直接失败难查得多。
var ErrIdentityConflict = errors.New("eresource: identity form field conflicts with the resolved identity")

// ErrIdentityMissing 解析出来的调用方身份缺少租户信息。
//
// 只在 access.require_identity 打开时返回。关闭时租户为空的请求会落到
// 「空租户」，读写都正常、测试也全绿，隔离却已经形同虚设——
// 这个开关就是为了把那种静默失效换成一次明确的 401。
var ErrIdentityMissing = errors.New("eresource: tenant identity is required")

// IdentityResolver 从请求中还原调用方身份。
//
// 这是本模块与接入方鉴权体系之间唯一的衔接点：模块不关心身份从哪来
// （网关注入的请求头、会话、JWT claims、内部 RPC 上下文都可以），
// 只关心拿到之后怎么用。默认实现是 HeaderIdentityResolver。
//
// 返回 error 表示「这个请求的身份不可信或不完整」，HTTP 层统一回 401；
// 返回 (nil, nil) 等价于一个各字段为空的 AccessRequest——它只能访问
// 同样没有租户信息的资源，而不是「放行一切」（见 manager.authorizeScope）。
//
// 实现必须保证返回值不含客户端可自由指定的原始输入：
// 取自 query / form / 未经网关剥除的请求头的租户 ID，
// 会让下游所有租户校验都只是在跟攻击者自己填的值做比较。
type IdentityResolver func(r *http.Request) (*model.AccessRequest, error)

// WithIdentityResolver 替换默认的请求头身份解析。
//
// 适用于服务自身已有鉴权中间件的场景：会话信息可直接从请求上下文读取，
// 绕一圈塞进请求头再读出来既多余、又多了一处配错就失效的环节。
//
//	transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config,
//	    transport.WithIdentityResolver(func(r *http.Request) (*model.AccessRequest, error) {
//	        session, ok := auth.FromContext(r.Context())
//	        if !ok {
//	            return nil, errors.New("unauthenticated")
//	        }
//	        return &model.AccessRequest{
//	            Namespace: session.Namespace,
//	            AccountID: session.AccountID,
//	            UserID:    session.UserID,
//	            IsAdmin:   session.HasRole("resource_admin"),
//	        }, nil
//	    }),
//	)
//
// 传 nil 时保持默认实现不变。
func WithIdentityResolver(resolver IdentityResolver) Option {
	return func(h *HTTPHandler) {
		if resolver != nil {
			h.resolveIdentity = resolver
		}
	}
}
