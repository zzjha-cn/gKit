package cdn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 阿里云 RPC 签名的官方示例向量。
//
// 用官方给的那组值钉死算法，而不是拿本实现自己算一遍再断言——
// 后者只能证明代码没变，证明不了代码是对的。
// 示例出自阿里云「签名机制」文档：AccessKeySecret 为 testsecret。
func TestRPCSignatureMatchesVendorVector(t *testing.T) {
	params := map[string]string{
		"AccessKeyId":      "testid",
		"Action":           "DescribeRegions",
		"Format":           "XML",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   "3ee8c1b8-83d3-44af-a94f-4e0ad82fd6cf",
		"SignatureVersion": "1.0",
		"Timestamp":        "2016-02-23T12:46:24Z",
		"Version":          "2014-05-26",
	}
	got := rpcSignature("GET", params, "testsecret")
	const want = "OLeaidS1JvxuMvnyHOwuJ+uX5qY="
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

// percentEncode 与 url.QueryEscape 的三处差异正是签名最常见的失败原因。
func TestPercentEncodeFollowsRFC3986(t *testing.T) {
	cases := map[string]string{
		"a b":  "a%20b", // 空格不能编码成 "+"
		"a*b":  "a%2Ab", // "*" 必须编码
		"a~b":  "a~b",   // "~" 不能编码
		"a/b":  "a%2Fb",
		"a=b&": "a%3Db%26",
	}
	for input, want := range cases {
		if got := percentEncode(input); got != want {
			t.Errorf("percentEncode(%q) = %q, want %q", input, got, want)
		}
	}
}

// 超过单批上限时必须自动切批：整批超限会被厂商直接拒掉，
// 而调用方是对着「刷新这些资源」的语义来的，不该被迫了解分页规则。
func TestRefreshSplitsIntoBatches(t *testing.T) {
	var mu sync.Mutex
	var batches []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		paths := r.Form.Get("ObjectPath")
		mu.Lock()
		batches = append(batches, len(strings.Split(paths, "\n")))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"RequestId":"x"}`))
	}))
	defer server.Close()

	refresher, err := NewAliCDNRefresher("ak", "sk", server.URL)
	if err != nil {
		t.Fatalf("new refresher: %v", err)
	}
	urls := make([]string, 0, 250)
	for i := 0; i < 250; i++ {
		urls = append(urls, "https://cdn.test/object")
	}
	if err = refresher.Refresh(context.Background(), urls); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 3 {
		t.Fatalf("sent %d batches, want 3", len(batches))
	}
	for _, size := range batches {
		if size > cdnBatchSize {
			t.Errorf("batch of %d exceeds the limit %d", size, cdnBatchSize)
		}
	}
}

// 厂商返回的 Code / Message 必须带出来，否则排查只能靠翻文档猜。
func TestRefreshSurfacesVendorError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"Code":"InvalidDomain.NotFound","Message":"domain not found"}`))
	}))
	defer server.Close()

	refresher, err := NewAliCDNRefresher("ak", "sk", server.URL)
	if err != nil {
		t.Fatalf("new refresher: %v", err)
	}
	err = refresher.Refresh(context.Background(), []string{"https://cdn.test/a"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "InvalidDomain.NotFound") ||
		!strings.Contains(err.Error(), "domain not found") {
		t.Fatalf("err = %v, want the vendor code and message surfaced", err)
	}
}

// 每次请求的 Nonce 必须不同，否则阿里云会把重复请求判成重放。
func TestEachRequestUsesAFreshNonce(t *testing.T) {
	var mu sync.Mutex
	nonces := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		nonces[r.Form.Get("SignatureNonce")] = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	refresher, _ := NewAliCDNRefresher("ak", "sk", server.URL)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := refresher.Refresh(ctx, []string{"https://cdn.test/a"}); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(nonces) != 3 {
		t.Fatalf("got %d distinct nonces across 3 requests", len(nonces))
	}
}
