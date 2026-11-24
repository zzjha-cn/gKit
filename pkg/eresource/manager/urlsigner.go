package manager

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// ErrCDNAuthKeyMissing CDN 鉴权模式缺少密钥。
// 正常情况下 Config.Validate 已经拦住，这里是第二道防线：
// 没有密钥时宁可报错，也不能签出一串谁都验不过的地址。
var ErrCDNAuthKeyMissing = errors.New("eresource: access.cdn_auth_key is not configured")

// URLSigner 生成受控资源的访问地址。
//
// 这一层**刻意不挂在 provider.Provider 上**。CDN 挡在任意源站前面，
// 和源站是 OSS 还是本地盘无关：一个本地盘做源站、前面挂 CDN 的部署完全合法，
// 而 LocalProvider 没有任何理由知道 CDN 的鉴权密钥。
// 把 CDN 鉴权塞进某个 Provider 实现，等于把一个边缘设施的配置
// 绑死在存储介质的选择上。
type URLSigner interface {
	// Name 返回模式标识，与 config 里的 access.url_mode 取值一致。
	Name() string
	// Sign 为对象生成带鉴权的访问地址。
	Sign(ctx context.Context, objectKey string, opt *model.URLSignOption) (string, error)
	// BindsUser 报告该模式能否把地址绑定到具体调用方。
	//
	// 返回 false 的模式不得用于 private 资源——地址一旦能转发给别人用，
	// private 与 protected 就没有区别了。约束实现在 signerFor，
	// 这里只负责如实申报能力。
	BindsUser() bool
}

// ossSigner 走 provider 自己的签名实现：
// local 签向本服务的验签路由（支持用户绑定），aliyun_oss 签 OSS 预签名地址。
type ossSigner struct {
	p      provider.Provider
	secret string
	scheme string
	host   string
}

func (s *ossSigner) Name() string { return config.URLModeOSSSigned }

// BindsUser 固定返回 true：这个模式**允许**绑定。
//
// 注意「允许」不等于「一定生效」——OSS 的签名地址直连 OSS、不经过本服务，
// 它没有校验请求方身份的位置，因此 AliOSSProvider.BuildSignedURL 会忽略
// BindUser。这是已知限制（README「已知限制」一节有记录），
// 需要真正绑定到人请走 /resource/{id}/content 代理下载。
func (s *ossSigner) BindsUser() bool { return true }

func (s *ossSigner) Sign(ctx context.Context, objectKey string, opt *model.URLSignOption) (string, error) {
	if opt == nil {
		return "", fmt.Errorf("eresource: url sign option is nil")
	}
	// provider 层收的是相对秒数，这里把绝对时间换回去。
	// 至少留 1 秒：换算出 0 或负数会让 provider 签出一条立刻过期的地址。
	expireSeconds := opt.ExpireAt - time.Now().Unix()
	if expireSeconds < 1 {
		expireSeconds = 1
	}
	return s.p.BuildSignedURL(ctx, objectKey, &model.SignedURLBuildOption{
		BindUser:      opt.BindUser,
		ExpireSeconds: expireSeconds,
		Scheme:        orDefault(opt.Scheme, s.scheme),
		Host:          orDefault(opt.Host, s.host),
		UserID:        opt.UserID,
		SecretKey:     s.secret,
		Process:       opt.Process,
	})
}

// cdnAuthRand 是 A 型鉴权签名串里的 rand 字段。
//
// 固定成 "0" 而不是真随机：rand 是 URL 的一部分，每次换一个值
// 缓存键就跟着变，CDN 命中率回到零——那正是切 CDN 鉴权要解决的问题。
// 阿里云不要求该字段随机，文档里给的示例就是 0。
const cdnAuthRand = "0"

// cdnAuthSigner 按阿里云 CDN 鉴权 A / B / C 型拼签名地址，由 CDN 边缘验签。
//
// 三型的签名串拼法按阿里云 CDN 鉴权文档实现。改动这段之前请对着
// 当时的官方文档再核一遍——拼错一个连接符的表现是全部 403，
// 而错误发生在边缘，业务侧只能看到客户端报错。
type cdnAuthSigner struct {
	mode   string
	key    string
	uid    string
	scheme string
	host   string
	p      provider.Provider
}

func (s *cdnAuthSigner) Name() string { return s.mode }

// BindsUser 固定返回 false。
//
// A 型签名串里虽然有 uid 字段，但 CDN 不校验它——写谁都一样。
// 把它当成绑定能力用，等于悄悄把 private 降级成可转发的 bearer token。
func (s *cdnAuthSigner) BindsUser() bool { return false }

func (s *cdnAuthSigner) Sign(_ context.Context, objectKey string, opt *model.URLSignOption) (string, error) {
	if opt == nil {
		return "", fmt.Errorf("eresource: url sign option is nil")
	}
	if s.key == "" {
		return "", ErrCDNAuthKeyMissing
	}
	// 借 provider 拼 base URL，逐段转义的逻辑只此一处。
	raw, err := s.p.BuildURL(objectKey, &model.URLBuildOption{
		Scheme: orDefault(opt.Scheme, s.scheme),
		Host:   orDefault(opt.Host, s.host),
	})
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if opt.Process != "" {
		query := parsed.Query()
		query.Set("x-oss-process", opt.Process)
		parsed.RawQuery = query.Encode()
	}
	uri := parsed.EscapedPath()
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}

	switch s.mode {
	case config.URLModeCDNAuthA:
		query := parsed.Query()
		query.Set("auth_key", cdnAuthKeyA(uri, opt.ExpireAt, cdnAuthRand, s.uid, s.key))
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	case config.URLModeCDNAuthB:
		return rebuildPath(parsed, cdnAuthPathB(uri, opt.ExpireAt, s.key)), nil
	case config.URLModeCDNAuthC:
		return rebuildPath(parsed, cdnAuthPathC(uri, opt.ExpireAt, s.key)), nil
	default:
		return "", fmt.Errorf("eresource: unsupported url mode %q", s.mode)
	}
}

// 下面三个函数各自实现一型鉴权的签名串拼法。
//
// 拆成纯函数不是为了复用，是为了**可验证**：它们的输入输出能直接对着
// 阿里云文档里的示例向量断言（见 cdnauth_test.go）。拼错一个连接符的
// 表现是边缘全部 403，而错误发生在 CDN 侧，业务服务这边一条日志都没有——
// 这种错误只能靠向量测试在上线前挡住。

// cdnAuthKeyA 生成 A 型的 auth_key 查询参数值。
//
//	sstring   = "<uri>-<timestamp>-<rand>-<uid>-<PrivateKey>"
//	auth_key  = "<timestamp>-<rand>-<uid>-" + md5(sstring)
func cdnAuthKeyA(uri string, expireAt int64, rand, uid, key string) string {
	ts := strconv.FormatInt(expireAt, 10)
	digest := md5Sum(fmt.Sprintf("%s-%s-%s-%s-%s", uri, ts, rand, uid, key))
	return fmt.Sprintf("%s-%s-%s-%s", ts, rand, uid, digest)
}

// cdnAuthPathB 生成 B 型的请求路径。
//
//	timestamp = yyyyMMddHHmm（精度到分钟）
//	sstring   = "<PrivateKey><timestamp><uri>"
//	path      = "/<timestamp>/" + md5(sstring) + uri
//
// 注意 B 型的时间戳精度只到分钟——它本身就带了一层天然对齐，
// 但粒度固定，不受 signed_url_align_sec 影响。
func cdnAuthPathB(uri string, expireAt int64, key string) string {
	ts := time.Unix(expireAt, 0).UTC().Format("200601021504")
	digest := md5Sum(key + ts + uri)
	return "/" + ts + "/" + digest + uri
}

// cdnAuthPathC 生成 C 型的请求路径。
//
//	timestamp = 十六进制的 Unix 时间戳
//	sstring   = "<PrivateKey><uri><timestamp>"
//	path      = "/" + md5(sstring) + "/<timestamp>" + uri
func cdnAuthPathC(uri string, expireAt int64, key string) string {
	ts := fmt.Sprintf("%x", expireAt)
	digest := md5Sum(key + uri + ts)
	return "/" + digest + "/" + ts + uri
}

// rebuildPath 用已经转义好的路径替换 URL 的 path 部分。
//
// 直接写 parsed.Path 会被 String() 再转义一次（%2F 之类），
// 因此走 Opaque 之外的唯一安全做法：同时设置 Path 与 RawPath，
// 让 net/url 认定 RawPath 是 Path 的合法转义形式。
func rebuildPath(parsed *url.URL, escaped string) string {
	unescaped, err := url.PathUnescape(escaped)
	if err != nil {
		unescaped = escaped
	}
	parsed.Path = unescaped
	parsed.RawPath = escaped
	return parsed.String()
}

func md5Sum(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// alignExpireAt 把绝对过期时间向上对齐到窗口边界。
//
// 对齐是 CDN 可缓存的前提：同一窗口内对同一对象签出的地址必须逐字节相同，
// 否则缓存键每次都变，切了 CDN 鉴权也等于没切。
//
// 向上取整而不是就近取整：取整后的有效期只会比调用方要求的更长，
// 不会出现「要了 5 分钟、实际只给了 10 秒」这种把地址提前签废的情况。
// 代价是实际有效期落在 [expireSeconds, expireSeconds+window) 区间里。
func alignExpireAt(now time.Time, expireSeconds, window int64) int64 {
	expireAt := now.Unix() + expireSeconds
	if window <= 0 {
		return expireAt
	}
	return ((expireAt + window - 1) / window) * window
}

// ErrProcessUnsupported 当前 provider 不具备媒体处理能力。
var ErrProcessUnsupported = errors.New("eresource: provider does not support media processing")

// ProcessedURL 签发一条带媒体处理参数的访问地址（缩略图、水印等）。
//
// 处理参数必须在**签发时**带上，不能让调用方拿到地址再自己拼：
//   - 签名地址里它要参与签名，附加上去只会让整条地址 403；
//   - 允许调用方自由拼参数，等于把图片处理的算力开放给任何持有链接的人，
//     而处理是按次计费的。
//
// CDN 鉴权模式下同样走得通：处理参数是 URL 的一部分，一起参与鉴权摘要，
// 不同参数自然就是不同的缓存键。
func (m *DefaultManager) ProcessedURL(ctx context.Context, id, process string, req *model.AccessRequest) (string, error) {
	resource, err := m.GetVerified(ctx, id, req)
	if err != nil {
		return "", err
	}
	if process == "" {
		return m.accessURLFor(ctx, resource, userIDOf(req))
	}
	if resource.AccessPolicy == model.AccessPolicyPublic {
		return m.provider.BuildURL(resource.ObjectKey, &model.URLBuildOption{
			Scheme:  m.config.Provider.Scheme,
			Host:    m.hostFor(resource.AccountID),
			Process: process,
		})
	}
	userID := userIDOf(req)
	signer := m.signerFor(resource.AccessPolicy)
	return signer.Sign(ctx, resource.ObjectKey, &model.URLSignOption{
		ExpireAt: m.resolveExpireAt(m.resolveExpire(0)),
		UserID:   userID,
		BindUser: m.bindSignedURL(resource.AccessPolicy, userID) && signer.BindsUser(),
		Process:  process,
		Host:     m.hostFor(resource.AccountID),
	})
}

func userIDOf(req *model.AccessRequest) string {
	if req == nil {
		return ""
	}
	return req.UserID
}
