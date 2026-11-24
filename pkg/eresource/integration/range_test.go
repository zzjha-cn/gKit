package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// rangeBody 受控下载测试共用的内容，每个字节都能看出自己的下标。
const rangeBody = "0123456789"

func uploadRangeFixture(t *testing.T, svc *eresource.Service) *model.Resource {
	t.Helper()
	result, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "clip.bin", Reader: strings.NewReader(rangeBody),
		Namespace: "crm", AccountID: "acct-a", BizType: "video",
		AccessPolicy: model.AccessPolicyProtected, Creator: "alice",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return result.Resource
}

func contentRequest(t *testing.T, router http.Handler, method, id, rangeHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, transport.RouteGroup+"/"+id+"/content", nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-a")
	req.Header.Set(transport.HeaderUserID, "alice")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return serve(router, req)
}

// T8：受控的音视频要能拖进度条、要能断点续传。
func TestContentServesByteRanges(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadRangeFixture(t, svc)
	router := newTestRouter(t, svc)

	cases := map[string]struct {
		header string
		want   string
		cr     string
	}{
		"闭区间":    {"bytes=2-5", "2345", "bytes 2-5/10"},
		"开区间":    {"bytes=5-", "56789", "bytes 5-9/10"},
		"后缀区间":   {"bytes=-3", "789", "bytes 7-9/10"},
		"首字节":    {"bytes=0-0", "0", "bytes 0-0/10"},
		"末尾超出对象": {"bytes=8-99", "89", "bytes 8-9/10"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := contentRequest(t, router, http.MethodGet, resource.ID, tc.header)
			if rec.Code != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206 (body=%s)", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != tc.want {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.want)
			}
			if got := rec.Header().Get("Content-Range"); got != tc.cr {
				t.Errorf("Content-Range = %q, want %q", got, tc.cr)
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(tc.want)) {
				t.Errorf("Content-Length = %q, want %d", got, len(tc.want))
			}
		})
	}
}

// 没有 Accept-Ranges，多数播放器与下载工具根本不会发 Range，
// 实现了区间读也等于没实现。
func TestContentAdvertisesRangeSupport(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadRangeFixture(t, svc)
	router := newTestRouter(t, svc)

	rec := contentRequest(t, router, http.MethodGet, resource.ID, "")
	if rec.Code != http.StatusOK || rec.Body.String() != rangeBody {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
}

// 区间整个落在对象之外是 RFC 要求必须回 416 的唯一情形，
// 且必须带上 Content-Range 告诉客户端真实长度，否则它无从纠正。
func TestContentRejectsUnsatisfiableRange(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadRangeFixture(t, svc)
	router := newTestRouter(t, svc)

	rec := contentRequest(t, router, http.MethodGet, resource.ID, "bytes=100-200")
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */10" {
		t.Errorf("Content-Range = %q, want bytes */10", got)
	}
}

// HTTP 允许服务端忽略 Range。多区间与语法不合法都走这条路：
// 客户端拿到 200 全量响应照样能工作，比报错友好得多。
func TestContentIgnoresUnsupportedRangeForms(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadRangeFixture(t, svc)
	router := newTestRouter(t, svc)

	for name, header := range map[string]string{
		"多区间":    "bytes=0-1,4-5",
		"语法不合法":  "bytes=abc",
		"单位不是字节": "items=0-1",
		"倒置区间":   "bytes=5-2",
	} {
		t.Run(name, func(t *testing.T) {
			rec := contentRequest(t, router, http.MethodGet, resource.ID, header)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if rec.Body.String() != rangeBody {
				t.Errorf("body = %q, want the whole object", rec.Body.String())
			}
		})
	}
}

// HEAD 只回响应头：播放器拿它探测体积，为此把整个对象拉一遍是纯粹的浪费。
func TestContentSupportsHEAD(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadRangeFixture(t, svc)
	router := newTestRouter(t, svc)

	rec := contentRequest(t, router, http.MethodHead, resource.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD must not carry a body, got %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(rangeBody)) {
		t.Errorf("Content-Length = %q, want %d", got, len(rangeBody))
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	// 安全响应头在 HEAD 上同样要有：客户端可能据此决定怎么处理后续的 GET。
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

// HEAD 与 Range 同样要受访问控制约束，不能成为绕过鉴权的旁路。
func TestHeadStillEnforcesAccessControl(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadRangeFixture(t, svc)
	router := newTestRouter(t, svc)

	req := httptest.NewRequest(http.MethodHead, transport.RouteGroup+"/"+resource.ID+"/content", nil)
	req.Header.Set(transport.HeaderNamespace, "crm")
	req.Header.Set(transport.HeaderAccountID, "acct-b") // 另一个租户
	if rec := serve(router, req); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant HEAD: status = %d, want 404", rec.Code)
	}
}

// 签名地址同样支持区间与 HEAD：它才是受控音视频真正的播放地址。
func TestSignedContentServesRangeAndHEAD(t *testing.T) {
	svc := newTestService(t, nil)
	resource := uploadRangeFixture(t, svc)
	router := newTestRouter(t, svc)

	signed, err := svc.Manager.GetAccessURL(context.Background(), resource.ID,
		&model.AccessRequest{Namespace: "crm", AccountID: "acct-a", UserID: "alice"})
	if err != nil {
		t.Fatalf("access url: %v", err)
	}
	path := strings.TrimPrefix(signed, "https://cdn.test")

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Range", "bytes=1-3")
	rec := serve(router, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "123" {
		t.Errorf("signed range: status = %d body = %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodHead, path, nil)
	if rec = serve(router, req); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("signed HEAD: status = %d body = %q", rec.Code, rec.Body.String())
	}
}
