package integration_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// 修复点 16/限流：超过窗口阈值后必须拒绝。
func TestRateLimitByResource(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPublic
		cfg.Access.RateLimitByResource = 2
		cfg.Access.RateLimitWindowSeconds = 60
	})
	result := upload(t, svc, "hot.txt", []byte("hot"))
	req := &model.AccessRequest{UserID: "u1", ClientIP: "10.0.0.1"}
	for i := 0; i < 2; i++ {
		if err := svc.Manager.VerifyAccess(context.Background(), result.Resource.ID, req); err != nil {
			t.Fatalf("request %d rejected: %v", i+1, err)
		}
	}
	if err := svc.Manager.VerifyAccess(context.Background(), result.Resource.ID, req); !errors.Is(err, manager.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

// ehttp 的 ClientIP() 会无条件返回 X-Forwarded-For 的最左值，而那正是客户端
// 自己写进去的。如果频控直接用它，攻击者每个请求换一个 XFF 就能让每次调用
// 落在不同的计数 key 上，IP 维度限流等于不存在。
func TestRateLimitIgnoresForgedForwardedFor(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Access.RateLimitByIP = 2
		cfg.Access.RateLimitWindowSeconds = 60
	})
	router := newTestRouter(t, svc)
	res := uploadForTenant(t, svc, "crm", "acct-a", "alice")

	accessOnce := func(forged string) int {
		req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+res.ID+"/access", nil)
		req.Header.Set(transport.HeaderNamespace, "crm")
		req.Header.Set(transport.HeaderAccountID, "acct-a")
		req.Header.Set("X-User-Id", "alice")
		req.Header.Set("X-Forwarded-For", forged)
		return serve(router, req).Code
	}

	// 每次伪造一个不同的来源 IP，但 TCP 对端始终是同一个
	if code := accessOnce("1.1.1.1"); code != http.StatusOK {
		t.Fatalf("第 1 次访问 status = %d，期望 200", code)
	}
	if code := accessOnce("2.2.2.2"); code != http.StatusOK {
		t.Fatalf("第 2 次访问 status = %d，期望 200", code)
	}
	if code := accessOnce("3.3.3.3"); code != http.StatusTooManyRequests {
		t.Fatalf("第 3 次访问 status = %d，期望 429——"+
			"换一个 X-Forwarded-For 就绕过了 IP 频控", code)
	}
}

// 网关注入的可信 IP 头必须生效，否则所有请求挤在 TCP 对端这一个计数桶里，
// 真实用户会互相挤占配额。
func TestTrustedClientIPHeaderSeparatesRateLimitBuckets(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Access.RateLimitByIP = 1
		cfg.Access.RateLimitWindowSeconds = 60
	})
	router := newTestRouter(t, svc)
	res := uploadForTenant(t, svc, "crm", "acct-a", "alice")

	accessFrom := func(trusted string) int {
		req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+res.ID+"/access", nil)
		req.Header.Set(transport.HeaderNamespace, "crm")
		req.Header.Set(transport.HeaderAccountID, "acct-a")
		req.Header.Set("X-User-Id", "alice")
		req.Header.Set(transport.HeaderClientIP, trusted)
		return serve(router, req).Code
	}

	if code := accessFrom("10.0.0.1"); code != http.StatusOK {
		t.Fatalf("用户 A 首次访问 status = %d，期望 200", code)
	}
	if code := accessFrom("10.0.0.2"); code != http.StatusOK {
		t.Fatalf("用户 B 首次访问 status = %d，期望 200——"+
			"可信 IP 头没生效，两个用户被算进了同一个计数桶", code)
	}
	if code := accessFrom("10.0.0.1"); code != http.StatusTooManyRequests {
		t.Fatalf("用户 A 第二次访问 status = %d，期望 429", code)
	}
}

// 不合法的 IP 字面量不能污染计数 key：网关配置错误时传过来的任意字符串
// 如果被原样当作 key，等于又开了一个绕过口子。
func TestInvalidTrustedClientIPFallsBackToPeerAddress(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Access.RateLimitByIP = 1
		cfg.Access.RateLimitWindowSeconds = 60
	})
	router := newTestRouter(t, svc)
	res := uploadForTenant(t, svc, "crm", "acct-a", "alice")

	accessWith := func(garbage string) int {
		req := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+res.ID+"/access", nil)
		req.Header.Set(transport.HeaderNamespace, "crm")
		req.Header.Set(transport.HeaderAccountID, "acct-a")
		req.Header.Set("X-User-Id", "alice")
		req.Header.Set(transport.HeaderClientIP, garbage)
		return serve(router, req).Code
	}

	if code := accessWith("not-an-ip-1"); code != http.StatusOK {
		t.Fatalf("首次访问 status = %d，期望 200", code)
	}
	if code := accessWith("not-an-ip-2"); code != http.StatusTooManyRequests {
		t.Fatalf("第二次访问 status = %d，期望 429——"+
			"非法 IP 头被当成了计数 key，换一个字符串就能重置配额", code)
	}
}

// 签名路由不查元数据、不走 verifyResourceAccess，防盗链与三维频控都覆盖不到它。
// 拿到一条签名地址就能在有效期内无限拉取，是刷带宽的主入口。
func TestSignedDownloadIsRateLimited(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
		cfg.Access.RateLimitByResource = 2
		cfg.Access.RateLimitWindowSeconds = 60
	})
	router := newTestRouter(t, svc)
	res := uploadForTenant(t, svc, "crm", "acct-a", "alice")

	signed, err := svc.Manager.GetAccessURL(context.Background(), res.ID, nil)
	if err != nil {
		t.Fatalf("build signed url: %v", err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}
	target := parsed.Path + "?" + parsed.RawQuery

	fetch := func() int {
		return serve(router, httptest.NewRequest(http.MethodGet, target, nil)).Code
	}

	if code := fetch(); code != http.StatusOK {
		t.Fatalf("第 1 次下载 status = %d，期望 200", code)
	}
	if code := fetch(); code != http.StatusOK {
		t.Fatalf("第 2 次下载 status = %d，期望 200", code)
	}
	if code := fetch(); code != http.StatusTooManyRequests {
		t.Fatalf("第 3 次下载 status = %d，期望 429——"+
			"一条签名地址可以无限次拉取", code)
	}
}

// T12：SetRateLimiter 是导出方法，约定在启动期调用挡不住运行期调用。
// 这个测试只在 -race 下有意义。
func TestRateLimiterCanBeReplacedConcurrently(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Access.RateLimitByIP = 1 << 30
	})
	resource := uploadOwned(t, svc, nil)
	mgr, ok := svc.Manager.(*manager.DefaultManager)
	if !ok {
		t.Fatalf("manager is %T, want *manager.DefaultManager", svc.Manager)
	}
	access := &model.AccessRequest{
		Namespace: "crm", AccountID: "acct-a", UserID: "alice", ClientIP: "10.0.0.1",
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = mgr.VerifyAccess(context.Background(), resource.ID, access)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			mgr.SetRateLimiter(allowAllLimiter{})
		}
	}()
	wg.Wait()
}

// allowAllLimiter 一个永远放行的频控实现，仅用于观察替换过程。
type allowAllLimiter struct{}

func (allowAllLimiter) Allow(string, int64) bool { return true }
