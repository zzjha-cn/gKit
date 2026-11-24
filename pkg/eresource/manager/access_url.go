package manager

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// DefaultSignedURLExpire 未配置 SignedURLDefaultExpire 时的签名有效期。
const DefaultSignedURLExpire int64 = 300

// ErrSignedURLSecretMissing 表示缺少签名密钥。
// 这里刻意不提供内置默认密钥：一个硬编码的公开默认值等于没有签名。
var ErrSignedURLSecretMissing = errors.New("eresource: access.signed_url_secret is not configured")

func (m *DefaultManager) signedURLSecret() (string, error) {
	if m.config.Access.SignedURLSecret == "" {
		return "", ErrSignedURLSecretMissing
	}
	return m.config.Access.SignedURLSecret, nil
}

func (m *DefaultManager) signedURLExpire() int64 {
	if m.config.Access.SignedURLDefaultExpire > 0 {
		return m.config.Access.SignedURLDefaultExpire
	}
	return DefaultSignedURLExpire
}

// DefaultSignedURLMaxExpire 未配置 SignedURLMaxExpire 时的兜底上限。
const DefaultSignedURLMaxExpire int64 = 86400

// signedURLMaxExpire 返回生效的有效期上限。
func (m *DefaultManager) signedURLMaxExpire() int64 {
	if m.config.Access.SignedURLMaxExpire > 0 {
		return m.config.Access.SignedURLMaxExpire
	}
	return max(DefaultSignedURLMaxExpire, m.signedURLExpire())
}

// resolveExpire 把调用方传入的有效期归一化：非正数取默认值，超过上限则钳制。
//
// 所有签名地址都必须过这里。签出去的地址无法撤销，上限是唯一的兜底——
// 少了它，客户端传一个 expire_seconds=315360000 就能自助签出一条
// 十年有效的直链，protected / private 两档策略当场失效。
//
// 超限按上限钳制而不是报错：调用方要的是一条「尽量长」的地址，
// 为此让整次上传失败不划算，钳制后的有效期本来就会原样回给它。
func (m *DefaultManager) resolveExpire(expireSeconds int64) int64 {
	if expireSeconds <= 0 {
		expireSeconds = m.signedURLExpire()
	}
	return min(expireSeconds, m.signedURLMaxExpire())
}

// VerifySignedObject 校验签名地址的摘要、用户绑定与有效期。
// 签名本身已绑定 object key，因此无需再查一次元数据即可判定请求合法。
//
// requesterID 是请求方的真实身份，只在地址声明了用户绑定（private 资源）
// 时才参与校验。传空值意味着「调用方无法识别请求者」，
// 绑定地址会因此被拒，而未绑定地址不受影响。
func (m *DefaultManager) VerifySignedObject(objectKey string, query url.Values, requesterID string) error {
	secretKey, err := m.signedURLSecret()
	if err != nil {
		return err
	}
	return provider.VerifySignedQuery(objectKey, query, secretKey, requesterID)
}

// signerFor 按资源的访问策略选择签名模式。
//
// private 永远回落到 provider 自带的签名器，忽略 access.url_mode：
// CDN 鉴权没有「绑定到人」的等价物（A 型的 uid 字段 CDN 并不校验），
// 让 private 走 CDN 等于悄悄把它降级成可转发的 bearer token。
// 这是代码里的硬约束而不是文档约定——配置写错只会让 private 少一层加速，
// 不会变成一次越权。
func (m *DefaultManager) signerFor(accessPolicy string) URLSigner {
	if accessPolicy == model.AccessPolicyPrivate && !m.signer.BindsUser() {
		return m.ossSigner
	}
	return m.signer
}

var (
	// ErrUnauthenticated 私有资源缺少调用方身份。
	ErrUnauthenticated = errors.New("eresource: private resource requires user authentication")
	// ErrHotlinkDenied 防盗链校验未通过。
	ErrHotlinkDenied = errors.New("eresource: access denied by hotlink protection policy")
	// ErrResourceNotVisible 资源属于其它租户。
	// 措辞刻意贴近「不存在」，transport 层也映射成 404，避免成为越权探测的预言机。
	ErrResourceNotVisible = errors.New("eresource: resource is not visible to the caller")
	// ErrNotResourceOwner 调用方不是该资源的上传者，无权修改或删除。
	ErrNotResourceOwner = errors.New("eresource: only the creator may modify or delete this resource")
)

// publicURL 构建公开直链。
func (m *DefaultManager) publicURL(objectKey string) (string, error) {
	return m.publicURLFor(objectKey, "")
}

// publicURLFor 按租户构建公开直链。
//
// 域名优先级：白标域名（按 account_id）> CDN 域名 > provider.domain。
// 白标排在最前是因为它最具体——给某个客户配了专属域名，
// 就不该再把他的资源地址发成平台的公共域名。
func (m *DefaultManager) publicURLFor(objectKey, accountID string) (string, error) {
	return m.provider.BuildURL(objectKey, &model.URLBuildOption{
		Scheme: m.config.Provider.Scheme,
		Host:   m.hostFor(accountID),
	})
}

// hostFor 解析某个租户应当使用的域名。
func (m *DefaultManager) hostFor(accountID string) string {
	if accountID != "" {
		if domain, ok := m.config.Provider.AccountDomains[accountID]; ok && domain != "" {
			return domain
		}
	}
	// 配了 CDN 域名就用它：public 是最该走 CDN 的一档，
	// 没有理由让它继续回源。都没配时回落 provider.domain。
	return orDefault(m.config.Access.CDNDomain, m.config.Provider.Domain)
}

// bindSignedURL 判断某个访问策略签出的地址是否应当绑定到具体用户。
//
// private 绑定、protected 不绑定，这让三档策略各自有了明确语义：
//   - public    固定直链，人人可取
//   - protected 签名地址是 bearer token，转发给谁谁能用（分享、嵌图场景）
//   - private   签名地址绑定到人，转发出去立刻失效
//
// userID 为空时无法绑定——绑到空身份的地址谁都匹配不上，等于签了一条
// 永远用不了的链接。走到这里且 userID 为空只有一种可能：req == nil 的
// 可信进程内调用（authorizeIdentity 已经挡掉了外部的匿名 private 访问），
// 对这类调用不绑定与「nil 表示跳过调用方相关校验」的约定一致。
func (m *DefaultManager) bindSignedURL(accessPolicy, userID string) bool {
	return accessPolicy == model.AccessPolicyPrivate && userID != ""
}

// signedURL 构建时效签名地址。
//
// accessPolicy 参与签名模式的选择（private 不接受 CDN 鉴权，见 signerFor），
// 因此不能省略——传空串会被当成非 private。
func (m *DefaultManager) signedURL(ctx context.Context, objectKey, accessPolicy, accountID, userID string, bindUser bool, expireSeconds int64) (string, error) {
	signer := m.signerFor(accessPolicy)
	// 密钥只有 provider 自签模式才需要；CDN 鉴权用的是另一把 key，
	// 在 Config.Validate 与 cdnAuthSigner 里各校验一次。
	if signer.Name() == config.URLModeOSSSigned {
		if _, err := m.signedURLSecret(); err != nil {
			return "", err
		}
	}
	expireAt := m.resolveExpireAt(m.resolveExpire(expireSeconds))
	return signer.Sign(ctx, objectKey, &model.URLSignOption{
		ExpireAt: expireAt,
		UserID:   userID,
		BindUser: bindUser && signer.BindsUser(),
		// 白标同样要覆盖签名地址：给某个客户配了专属域名，
		// 却把他的受控链接发成平台域名，等于白标只做了一半。
		Host: m.hostFor(accountID),
	})
}

// DefaultRedirectExpireSeconds 大文件 302 分流时签名地址的有效期。
//
// 刻意很短：这条地址一旦签出就是可转发的 bearer token，
// 它存在的时间就是安全边界让步的时间。30 秒足够客户端跟进一次跳转，
// 不够拿去分享。
const DefaultRedirectExpireSeconds int64 = 30

// RedirectURL 为 private 大文件的 302 分流签发一条短时效地址。
//
// 与 GetAccessURL 的两点不同，都是刻意的：
//  1. **不走时间窗对齐**。对齐是为了可缓存，而这里要的恰恰是尽快失效，
//     两个目标正好相反——共用一套配置会让这条地址被拖长到一个窗口。
//  2. 有效期固定而不是取配置的默认值：配置里那个值是按「嵌在页面上的图」
//     定的，用在这里等于把让步窗口放大几十倍。
func (m *DefaultManager) RedirectURL(ctx context.Context, resource *model.Resource, userID string) (string, error) {
	signer := m.signerFor(resource.AccessPolicy)
	expireAt := time.Now().Unix() + DefaultRedirectExpireSeconds
	return signer.Sign(ctx, resource.ObjectKey, &model.URLSignOption{
		ExpireAt: expireAt,
		UserID:   userID,
		BindUser: m.bindSignedURL(resource.AccessPolicy, userID) && signer.BindsUser(),
		Host:     m.hostFor(resource.AccountID),
	})
}

// ShouldRedirectContent 判断一次受控下载是否应当改走 302 分流。
//
// 只对 private 生效：protected 本来就能直接签地址给客户端，
// public 更是直链，两者都不经过代理读流，没有可分流的流量。
func (m *DefaultManager) ShouldRedirectContent(resource *model.Resource) bool {
	threshold := m.config.Access.PrivateRedirectMinBytes
	if threshold <= 0 || resource == nil {
		return false
	}
	if resource.AccessPolicy != model.AccessPolicyPrivate {
		return false
	}
	return resource.Size >= threshold
}

// resolveExpireAt 把相对有效期换成（可能对齐过的）绝对过期时间。
func (m *DefaultManager) resolveExpireAt(expireSeconds int64) int64 {
	return alignExpireAt(time.Now(), expireSeconds, m.config.Access.SignedURLAlignSec)
}

// accessURLFor 按资源的访问策略选择公开直链或签名地址。
func (m *DefaultManager) accessURLFor(ctx context.Context, resource *model.Resource, userID string) (string, error) {
	if resource.AccessPolicy == model.AccessPolicyPublic {
		// 刻意不复用 resource.URL：库里那一份是上传当时的域名快照。
		// 读的时候重新拼，换 CDN 域名 / 切 HTTPS / 配白标域名
		// 才不需要刷库——这是 B13 的全部内容。
		return m.publicURLFor(resource.ObjectKey, resource.AccountID)
	}
	return m.signedURL(ctx, resource.ObjectKey, resource.AccessPolicy, resource.AccountID, userID,
		m.bindSignedURL(resource.AccessPolicy, userID), 0)
}

// fillURL 按当前配置回填对外地址。
//
// public 资源每次读取都重算；非 public 资源的 URL 字段一律为空——
// 固定直链留在元数据里，等于每次 Get / List 都把底层地址交出去。
func (m *DefaultManager) fillURL(resource *model.Resource) *model.Resource {
	if resource == nil {
		return nil
	}
	if resource.AccessPolicy != model.AccessPolicyPublic {
		resource.URL = ""
		return resource
	}
	if url, err := m.publicURLFor(resource.ObjectKey, resource.AccountID); err == nil {
		resource.URL = url
	}
	return resource
}

// uploadAccessURL 生成上传结果里返回给调用方的地址，
// 并在受控场景下记录本次签名的过期时间。
func (m *DefaultManager) uploadAccessURL(ctx context.Context, resource *model.Resource, req *model.UploadRequest) (string, error) {
	if resource.AccessPolicy == model.AccessPolicyPublic {
		// 按当前配置重算，不读 resource.URL——那一份现在已经不落库了（B13）。
		return m.publicURLFor(resource.ObjectKey, resource.AccountID)
	}
	expireSeconds := m.resolveExpire(req.ExpireSeconds)
	signed, err := m.signedURL(ctx, resource.ObjectKey, resource.AccessPolicy, resource.AccountID, req.Creator,
		m.bindSignedURL(resource.AccessPolicy, req.Creator), expireSeconds)
	if err != nil {
		return "", err
	}
	// 记录对齐之后的真实过期时间，而不是请求的相对秒数——
	// 开了 signed_url_align_sec 时两者会差最多一个窗口，
	// 记成相对值会让元数据与地址实际失效时间对不上。
	resource.SignedURLExpireAt = time.Unix(m.resolveExpireAt(expireSeconds), 0)
	return signed, nil
}
