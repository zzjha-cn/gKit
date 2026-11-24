package integration_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// 修复点 8：受控资源不得把公开直链写进元数据。
func TestControlledResourceDoesNotPersistPublicURL(t *testing.T) {
	svc := newTestService(t, nil)
	for _, policy := range []string{model.AccessPolicyProtected, model.AccessPolicyPrivate} {
		result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
			FileName: "secret.pdf", Reader: strings.NewReader("secret"),
			AccessPolicy: policy, Creator: "alice", ExpireSeconds: 120,
		})
		if err != nil {
			t.Fatalf("upload %s: %v", policy, err)
		}
		if result.Resource.URL != "" {
			t.Errorf("%s resource persisted url %q", policy, result.Resource.URL)
		}
		// 返回给调用方的必须是签名地址，且过期时间被记录下来
		if !strings.Contains(result.URL, provider.SignParamSign+"=") {
			t.Errorf("%s result url %q is not signed", policy, result.URL)
		}
		if result.Resource.SignedURLExpireAt.IsZero() {
			t.Errorf("%s resource did not record signed url expiry", policy)
		}
		stored, err := svc.Manager.Get(context.Background(), result.Resource.ID, nil)
		if err != nil {
			t.Fatalf("get %s: %v", policy, err)
		}
		if stored.URL != "" {
			t.Errorf("%s stored url = %q, want empty", policy, stored.URL)
		}
	}

	// public 资源仍然保留固定直链
	pub, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "logo.txt", Reader: strings.NewReader("logo"), AccessPolicy: model.AccessPolicyPublic,
	})
	if err != nil {
		t.Fatalf("public upload: %v", err)
	}
	if pub.Resource.URL == "" || pub.URL != pub.Resource.URL {
		t.Errorf("public resource should keep a stable url, got %q / %q", pub.Resource.URL, pub.URL)
	}
}

// 修复点 3：签名必须是真 HMAC，且改一个字节就失效。
func TestSignedURLVerification(t *testing.T) {
	const key, secret = "image/2026/09/21/abc.png", "unit-test-secret"
	query := provider.SignedQuery(key, timeFuture(), "u1", false, secret)

	if err := provider.VerifySignedQuery(key, query, secret, ""); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := provider.VerifySignedQuery(key, query, "other-secret", ""); !errors.Is(err, provider.ErrSignatureInvalid) {
		t.Errorf("wrong secret: err = %v, want ErrSignatureInvalid", err)
	}
	if err := provider.VerifySignedQuery("image/2026/09/21/other.png", query, secret, ""); !errors.Is(err, provider.ErrSignatureInvalid) {
		t.Errorf("object key swap: err = %v, want ErrSignatureInvalid", err)
	}

	tampered := cloneQuery(query)
	tampered.Set(provider.SignParamUser, "u2")
	if err := provider.VerifySignedQuery(key, tampered, secret, ""); !errors.Is(err, provider.ErrSignatureInvalid) {
		t.Errorf("user tamper: err = %v, want ErrSignatureInvalid", err)
	}

	// 延长过期时间而不重算签名，必须被识破
	extended := cloneQuery(query)
	extended.Set(provider.SignParamExpire, "99999999999")
	if err := provider.VerifySignedQuery(key, extended, secret, ""); !errors.Is(err, provider.ErrSignatureInvalid) {
		t.Errorf("expire tamper: err = %v, want ErrSignatureInvalid", err)
	}

	expired := provider.SignedQuery(key, 1, "u1", false, secret)
	if err := provider.VerifySignedQuery(key, expired, secret, ""); !errors.Is(err, provider.ErrSignatureExpired) {
		t.Errorf("expired: err = %v, want ErrSignatureExpired", err)
	}

	missing := url.Values{provider.SignParamExpire: []string{"99999999999"}}
	if err := provider.VerifySignedQuery(key, missing, secret, ""); !errors.Is(err, provider.ErrSignatureMissing) {
		t.Errorf("missing: err = %v, want ErrSignatureMissing", err)
	}
}

// 签名 URL 必须挂在验签路由下，而不是静态目录的公开路径。
func TestSignedURLTargetsVerifiedRoute(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
		cfg.Provider.BasePath = "static"
	})
	result := upload(t, svc, "doc.pdf", []byte("pdf-bytes"))
	signed, err := svc.Manager.GetAccessURL(context.Background(), result.Resource.ID, &model.AccessRequest{UserID: "u1"})
	if err != nil {
		t.Fatalf("get access url: %v", err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}
	// 必须按前缀判断而不是子串包含：base_path 现在是 object key 的一部分，
	// 签名地址里**必然**出现 "static" 这一段（/resource/signed/static/...）。
	// 真正要守的是「不会被 nginx 的 location /static/ 静态直出」，
	// 而 location 匹配的是路径前缀，所以这里也只能比前缀。
	if !strings.HasPrefix(parsed.Path, "/"+config.DefaultSignedBasePath+"/") {
		t.Errorf("signed url %q should point at the verified route", signed)
	}
	if strings.HasPrefix(parsed.Path, "/static/") {
		t.Errorf("signed url %q must not be served by the public static location", signed)
	}
	if err = svc.Manager.VerifySignedObject(result.Resource.ObjectKey, parsed.Query(), ""); err != nil {
		t.Errorf("generated signed url failed verification: %v", err)
	}
}

// protected 的签名地址保持 bearer token 语义：分享、嵌图场景依赖它可转发。
func TestProtectedSignedURLStaysTransferable(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)
	res := uploadWithPolicy(t, svc, model.AccessPolicyProtected, "alice")
	path, query := signedURLFor(t, svc, res, "alice")

	if query.Has(provider.SignParamBind) {
		t.Fatalf("protected 签名地址不应带绑定标记，query = %v", query)
	}

	req := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
	req.Header.Set(transport.HeaderUserID, "bob")
	if rec := serve(router, req); rec.Code != http.StatusOK {
		t.Errorf("他人访问 protected 签名地址 status = %d，期望 200——"+
			"绑定不该扩散到 protected，会打断分享与嵌图场景", rec.Code)
	}
}

// 绑定标记本身必须受签名保护：摘掉 b=1 不能把绑定地址降级成可转发地址。
func TestStrippingBindFlagInvalidatesSignature(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)
	res := uploadWithPolicy(t, svc, model.AccessPolicyPrivate, "alice")
	path, query := signedURLFor(t, svc, res, "alice")

	stripped := url.Values{}
	for k, v := range query {
		if k == provider.SignParamBind {
			continue
		}
		stripped[k] = v
	}
	req := httptest.NewRequest(http.MethodGet, path+"?"+stripped.Encode(), nil)
	req.Header.Set(transport.HeaderUserID, "bob")
	if rec := serve(router, req); rec.Code != http.StatusForbidden {
		t.Errorf("摘掉 b 参数后 status = %d，期望 403——"+
			"绑定标记没进签名，可以被降级成可转发地址", rec.Code)
	}

	// 改 u 想冒充别人同样必须失败
	impersonated := url.Values{}
	for k, v := range query {
		impersonated[k] = v
	}
	impersonated.Set(provider.SignParamUser, "bob")
	req = httptest.NewRequest(http.MethodGet, path+"?"+impersonated.Encode(), nil)
	req.Header.Set(transport.HeaderUserID, "bob")
	if rec := serve(router, req); rec.Code != http.StatusForbidden {
		t.Errorf("篡改 u 后 status = %d，期望 403", rec.Code)
	}
}

// b 参数只认 "1"：写别的值不能被当成「未绑定」静默放行。
func TestUnknownBindValueIsRejected(t *testing.T) {
	secret := "unit-test-secret"
	key := "crm/acct-a/doc/2026/09/abc-1234.txt"
	query := provider.SignedQuery(key, timeFuture(), "alice", true, secret)

	for _, bogus := range []string{"true", "0", "yes", "01", ""} {
		tampered := url.Values{}
		for k, v := range query {
			tampered[k] = v
		}
		tampered.Set(provider.SignParamBind, bogus)
		err := provider.VerifySignedQuery(key, tampered, secret, "alice")
		if !errors.Is(err, provider.ErrSignatureInvalid) {
			t.Errorf("b=%q 时 err = %v，期望 ErrSignatureInvalid——"+
				"未知取值被当成了未绑定", bogus, err)
		}
	}
}

// 绑定到空身份的地址谁都匹配不上，等于签了一条永远用不了的链接，必须退化成不绑定。
func TestBindWithoutUserDegradesToUnbound(t *testing.T) {
	secret := "unit-test-secret"
	key := "crm/acct-a/doc/2026/09/abc-1234.txt"
	query := provider.SignedQuery(key, timeFuture(), "", true, secret)

	if query.Has(provider.SignParamBind) {
		t.Fatalf("userID 为空时不应标记绑定，query = %v", query)
	}
	if err := provider.VerifySignedQuery(key, query, secret, ""); err != nil {
		t.Errorf("无身份签发的地址无法使用：%v", err)
	}
}

// 引入绑定之前签发的地址（不带 b 参数）必须继续有效，否则升级瞬间所有在途链接全挂。
func TestUnboundSignatureFormatIsUnchanged(t *testing.T) {
	secret := "unit-test-secret"
	key := "crm/acct-a/doc/2026/09/abc-1234.txt"
	expireAt := timeFuture()

	query := provider.SignedQuery(key, expireAt, "alice", false, secret)
	if query.Has(provider.SignParamBind) {
		t.Fatalf("未绑定地址不应出现 b 参数")
	}
	// 老版本的摘要等价于 bindUser=false 的摘要
	legacy := provider.SignObject(key, expireAt, "alice", false, secret)
	if query.Get(provider.SignParamSign) != legacy {
		t.Fatalf("未绑定地址的签名格式发生了变化，老链接会在升级后立刻失效")
	}
	if err := provider.VerifySignedQuery(key, query, secret, ""); err != nil {
		t.Errorf("未绑定地址在没有请求方身份时被拒绝：%v", err)
	}
}

// T2：签出去的地址无法撤销，有效期必须有天花板。
// 没有上限时，客户端一个表单字段就能给自己签一条十年有效的永久直链。
func TestSignedURLExpireIsCapped(t *testing.T) {
	const maxExpire int64 = 600
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
		cfg.Access.SignedURLDefaultExpire = 60
		cfg.Access.SignedURLMaxExpire = maxExpire
	})

	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "note.txt", Reader: strings.NewReader("capped"),
		ExpireSeconds: 315360000, // 10 年
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := signedExpireAt(t, result.URL) - time.Now().Unix(); got > maxExpire+5 {
		t.Errorf("签出地址有效期 = %ds，期望被钳制到 %ds 以内", got, maxExpire)
	}
	// 元数据里记录的过期时间也必须是钳制后的值，否则对外的口径对不上
	if got := time.Until(result.Resource.SignedURLExpireAt); got > time.Duration(maxExpire+5)*time.Second {
		t.Errorf("SignedURLExpireAt 距今 = %v，期望不超过 %ds", got, maxExpire)
	}

	// 上限之内的请求值原样生效
	result, err = svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "small.txt", Reader: strings.NewReader("within"), ExpireSeconds: 120,
	})
	if err != nil {
		t.Fatalf("upload within cap: %v", err)
	}
	if got := signedExpireAt(t, result.URL) - time.Now().Unix(); got < 100 || got > 130 {
		t.Errorf("上限内的有效期 = %ds，期望约 120s", got)
	}
}

// 上限小于默认有效期是自相矛盾的配置，必须在启动阶段就报出来。
func TestSignedURLMaxExpireMustNotBeLessThanDefault(t *testing.T) {
	cfg := config.Config{
		Manager:  config.ManagerConfig{DefaultAccessPolicy: model.AccessPolicyPublic},
		Provider: config.ProviderConfig{Type: provider.TypeLocal, RootDir: t.TempDir(), Domain: "cdn.test"},
		Access:   config.AccessConfig{SignedURLDefaultExpire: 3600, SignedURLMaxExpire: 60},
	}
	cfg.Normalize()
	if err := cfg.Validate(); !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestWhiteLabelDomainPerAccount(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(dir)
	cfg.Provider.AccountDomains = map[string]string{"acct-vip": "assets.vipbrand.com"}
	svc, err := eresource.New(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	ctx := context.Background()

	vip := uploadForTenant(t, svc, "crm", "acct-vip", "u-1")
	plain := uploadForTenant(t, svc, "crm", "acct-plain", "u-2")

	vipURL, err := svc.Manager.GetAccessURL(ctx, vip.ID, nil)
	if err != nil {
		t.Fatalf("vip access url: %v", err)
	}
	if !strings.Contains(vipURL, "assets.vipbrand.com") {
		t.Errorf("vip url = %q, want the white-label domain", vipURL)
	}
	plainURL, err := svc.Manager.GetAccessURL(ctx, plain.ID, nil)
	if err != nil {
		t.Fatalf("plain access url: %v", err)
	}
	if !strings.Contains(plainURL, "cdn.test") {
		t.Errorf("plain url = %q, want the default domain", plainURL)
	}

	// public 资源读接口回填的 URL 也要用白标域名，
	// 否则前端拿到的还是平台域名。
	pub, err := svc.Manager.Upload(ctx, &model.UploadRequest{
		FileName: "logo.png", Reader: bytes.NewReader(pngBytes()),
		Namespace: "crm", AccountID: "acct-vip", BizType: "image",
		AccessPolicy: model.AccessPolicyPublic,
	})
	if err != nil {
		t.Fatalf("upload public: %v", err)
	}
	fetched, err := svc.Manager.Get(ctx, pub.Resource.ID, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(fetched.URL, "assets.vipbrand.com") {
		t.Errorf("resource url = %q, want the white-label domain", fetched.URL)
	}
}

// 加一个白标客户不该需要刷存量数据——这正是 B13 解耦换来的。
func TestWhiteLabelAppliesToExistingResources(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(dir)
	svc, err := eresource.New(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	created, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "logo.png", Reader: bytes.NewReader(pngBytes()),
		Namespace: "crm", AccountID: "acct-vip", BizType: "image",
		AccessPolicy: model.AccessPolicyPublic,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	existing := created.Resource
	svc.Close()

	cfg.Provider.AccountDomains = map[string]string{"acct-vip": "assets.vipbrand.com"}
	rebranded, err := eresource.New(cfg)
	if err != nil {
		t.Fatalf("new service after rebrand: %v", err)
	}
	t.Cleanup(rebranded.Close)

	got, err := rebranded.Manager.Get(context.Background(), existing.ID, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(got.URL, "assets.vipbrand.com") {
		t.Fatalf("url = %q, want the white-label domain without any migration", got.URL)
	}
}

// 换域名之后，已经存在的 public 资源必须返回新域名而不是上传时的快照。
// 这是 B13 的全部要点：地址存 object key、读时拼域名，换 CDN 不用刷库。
func TestPublicURLFollowsCurrentDomain(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(dir)
	cfg.Provider.Domain = "old.example.com"

	svc, err := eresource.New(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	result := upload(t, svc, "logo.png", pngBytes())
	id := result.Resource.ID
	if !strings.Contains(result.URL, "old.example.com") {
		t.Fatalf("upload url = %q, want the old domain", result.URL)
	}
	svc.Close()

	// 同一份元数据文件，只换域名重新起一个实例——等价于线上改配置重启。
	cfg.Provider.Domain = "new.example.com"
	moved, err := eresource.New(cfg)
	if err != nil {
		t.Fatalf("new service after domain change: %v", err)
	}
	t.Cleanup(moved.Close)

	got, err := moved.Manager.Get(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(got.URL, "new.example.com") {
		t.Fatalf("resource url = %q, want the new domain (no DB migration should be needed)", got.URL)
	}

	accessURL, err := moved.Manager.GetAccessURL(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("access url: %v", err)
	}
	if !strings.Contains(accessURL, "new.example.com") {
		t.Fatalf("access url = %q, want the new domain", accessURL)
	}
}

// 直链不再落库：库里留一份域名快照，换域名就得刷库。
func TestPublicURLIsNotPersisted(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "logo.png", pngBytes())
	if result.URL == "" {
		t.Fatal("upload should still return a usable url")
	}
	raw := readMetaFile(t, svc)
	if strings.Contains(raw, "cdn.test") {
		t.Errorf("metadata file still persists a domain snapshot:\n%s", raw)
	}
}

// 对齐窗口内签出的地址必须逐字节相同，否则 CDN 缓存键每次都变，
// 切 cdn_auth_* 等于白切。
func TestSignedURLIsStableWithinAlignWindow(t *testing.T) {
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
		c.Access.SignedURLAlignSec = 3600
	})
	result := upload(t, svc, "poster.png", pngBytes())

	first, err := svc.Manager.GetAccessURL(context.Background(), result.Resource.ID, nil)
	if err != nil {
		t.Fatalf("first sign: %v", err)
	}
	second, err := svc.Manager.GetAccessURL(context.Background(), result.Resource.ID, nil)
	if err != nil {
		t.Fatalf("second sign: %v", err)
	}
	if first != second {
		t.Fatalf("aligned urls differ:\n  %s\n  %s", first, second)
	}
}

// 不开对齐时保持原有行为：过期时间跟着当下走。
func TestSignedURLWithoutAlignKeepsRequestedExpiry(t *testing.T) {
	svc := newTestService(t, func(c *config.Config) {
		c.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
		c.Access.SignedURLDefaultExpire = 60
	})
	result := upload(t, svc, "poster.png", pngBytes())
	signed, err := svc.Manager.GetAccessURL(context.Background(), result.Resource.ID, nil)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Query().Get("t") == "" {
		t.Fatalf("signed url lost its expiry parameter: %s", signed)
	}
}

func cloneQuery(src url.Values) url.Values {
	dst := url.Values{}
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	return dst
}

func timeFuture() int64 { return 1<<31 - 1 }

// signedURLFor 以指定用户身份签发访问地址，返回可直接请求的 path?query。
func signedURLFor(t *testing.T, svc *eresource.Service, res *model.Resource, userID string) (string, url.Values) {
	t.Helper()
	raw, err := svc.Manager.GetAccessURL(context.Background(), res.ID, &model.AccessRequest{
		Namespace: res.Namespace, AccountID: res.AccountID, UserID: userID,
	})
	if err != nil {
		t.Fatalf("get access url: %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}
	return parsed.Path, parsed.Query()
}

// signedExpireAt 从签名地址里取出过期时间戳。
func signedExpireAt(t *testing.T, signed string) int64 {
	t.Helper()
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed url %q: %v", signed, err)
	}
	raw := parsed.Query().Get(provider.SignParamExpire)
	expireAt, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("parse expire %q: %v", raw, err)
	}
	return expireAt
}
