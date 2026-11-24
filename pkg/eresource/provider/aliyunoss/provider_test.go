package aliyunoss

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// 对象不存在必须靠错误类型判断，而不是匹配错误文本。
func TestIsNotFoundErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"404 status", oss.ServiceError{StatusCode: http.StatusNotFound}, true},
		{"NoSuchKey", oss.ServiceError{StatusCode: http.StatusNotFound, Code: "NoSuchKey"}, true},
		{"NoSuchBucket", oss.ServiceError{StatusCode: http.StatusNotFound, Code: "NoSuchBucket"}, true},
		{"wrapped 404", fmt.Errorf("get meta: %w", oss.ServiceError{StatusCode: http.StatusNotFound}), true},
		{"unexpected status 404", oss.CheckRespCode(http.StatusNotFound, []int{http.StatusOK}), true},
		{"403 denied", oss.ServiceError{StatusCode: http.StatusForbidden, Code: "AccessDenied"}, false},
		{"500 server error", oss.ServiceError{StatusCode: http.StatusInternalServerError}, false},
		{"unexpected status 500", oss.CheckRespCode(http.StatusInternalServerError, []int{http.StatusOK}), false},
		{"plain error", errors.New("dial tcp: connection refused"), false},
		// 文本匹配会把这条误判成「不存在」，类型判断不会
		{"message mentions NoSuchKey", errors.New("audit log: user searched NoSuchKey"), false},
	}
	for _, c := range cases {
		if got := isNotFoundErr(c.err); got != c.want {
			t.Errorf("%s: isNotFoundErr = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestObjectStatFromHeader(t *testing.T) {
	header := http.Header{}
	header.Set("Content-Length", "2048")
	header.Set("Content-Type", "image/png")
	header.Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")

	stat := objectStatFromHeader(header)
	if !stat.Exists {
		t.Error("stat.Exists should be true")
	}
	if stat.Size != 2048 {
		t.Errorf("Size = %d, want 2048", stat.Size)
	}
	if stat.ContentType != "image/png" {
		t.Errorf("ContentType = %q", stat.ContentType)
	}
	want := time.Date(2015, 10, 21, 7, 28, 0, 0, time.UTC)
	if !stat.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v", stat.UpdatedAt, want)
	}
}

// 头缺失或格式非法时不能 panic，也不能写入垃圾值。
func TestObjectStatFromHeaderToleratesMissingFields(t *testing.T) {
	stat := objectStatFromHeader(http.Header{})
	if stat.Size != 0 || !stat.UpdatedAt.IsZero() || stat.ContentType != "" {
		t.Errorf("unexpected stat for empty header: %+v", stat)
	}

	bad := http.Header{}
	bad.Set("Content-Length", "not-a-number")
	bad.Set("Last-Modified", "yesterday")
	stat = objectStatFromHeader(bad)
	if stat.Size != 0 || !stat.UpdatedAt.IsZero() {
		t.Errorf("malformed headers should be ignored, got %+v", stat)
	}
}

// 推断不出合法地域时必须返回空串让调用方显式报错，
// 而不是把 "oss-" 后面的内容原样当成 region。
func TestExtractRegion(t *testing.T) {
	cases := map[string]string{
		"oss-cn-hangzhou.aliyuncs.com":          "cn-hangzhou",
		"https://oss-cn-hangzhou.aliyuncs.com":  "cn-hangzhou",
		"oss-us-west-1.aliyuncs.com":            "us-west-1",
		"oss-ap-southeast-2.aliyuncs.com":       "ap-southeast-2",
		"oss-cn-hangzhou-internal.aliyuncs.com": "cn-hangzhou",
		"oss-cn-shenzhen.aliyuncs.com:443":      "cn-shenzhen",
		"OSS-CN-BEIJING.aliyuncs.com":           "cn-beijing",
		"oss-accelerate.aliyuncs.com":           "",
		"oss-accelerate-overseas.aliyuncs.com":  "",
		"cdn.example.com":                       "",
		"":                                      "",
	}
	for endpoint, want := range cases {
		if got := extractRegion(endpoint); got != want {
			t.Errorf("extractRegion(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

// 身份字段必须严格校验，不能静默剥离非法字符。
func TestObjectACL(t *testing.T) {
	cases := map[string]oss.ACLType{
		model.AccessPolicyPublic:    oss.ACLPublicRead,
		model.AccessPolicyProtected: oss.ACLPrivate,
		model.AccessPolicyPrivate:   oss.ACLPrivate,
		// 认不出的策略必须失败在「访问不到」一侧，绝不能放成公开
		"":            oss.ACLPrivate,
		"PUBLIC":      oss.ACLPrivate,
		"public-read": oss.ACLPrivate,
		"anything":    oss.ACLPrivate,
	}
	for policy, want := range cases {
		if got := objectACL(policy); got != want {
			t.Errorf("objectACL(%q) = %q, want %q", policy, got, want)
		}
	}
}

// OSS 直链要和本地 provider 一样逐段转义：object key 里的空格、#、? 原样拼进 URL
// 会被浏览器截断或解释成 fragment / query，拿到的是另一个对象或者 404。
func TestAliOSSBuildURLEscapesSegments(t *testing.T) {
	p := &AliOSSProvider{bucketName: "b", endpoint: "oss-cn-hangzhou.aliyuncs.com", scheme: "https"}
	got, err := p.BuildURL("ns/acct/doc/2026/09/a b#c?d.txt", nil)
	if err != nil {
		t.Fatalf("build url: %v", err)
	}
	want := "https://b.oss-cn-hangzhou.aliyuncs.com/ns/acct/doc/2026/09/a%20b%23c%3Fd.txt"
	if got != want {
		t.Errorf("BuildURL = %q\n want %q", got, want)
	}

	// 自定义域名同样走转义
	p.domain = "cdn.example.com"
	got, _ = p.BuildURL("/ns/x y.png/", nil)
	if want := "https://cdn.example.com/ns/x%20y.png"; got != want {
		t.Errorf("BuildURL with domain = %q, want %q", got, want)
	}
}

func TestBasePathIsAStoragePrefix(t *testing.T) {
	p := &AliOSSProvider{basePath: "static"}
	req := &model.UploadRequest{
		Namespace: "crm", AccountID: "acct-a", BizType: "doc",
		Hash: "d41d8cd98f00b204e9800998ecf8427e", Ext: ".png",
	}
	key, err := p.objectKey(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "static/crm/acct-a/doc/") {
		t.Fatalf("object key = %q, want storage prefix", key)
	}
	link, err := p.BuildURL(key, &model.URLBuildOption{Scheme: "https", Host: "cdn.test"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(link, "/static/") != 1 || !strings.HasPrefix(link, "https://cdn.test/static/crm/acct-a/doc/") {
		t.Errorf("URL = %q, want storage prefix exactly once", link)
	}
}
