package manager

import (
	"strings"
	"testing"
	"time"
)

// 阿里云 CDN 鉴权文档里的示例向量。
//
// 这是 Roadmap §2.8 要求的那组测试，和「形态测试」的区别很实在：
// 形态测试只能证明签出来的东西长得像签名，证明不了**拼接顺序是对的**。
// 而拼错一个连接符的表现是边缘全部 403——错误发生在 CDN 侧，
// 业务服务这边一条日志都没有，上线前不挡住就只能等客户报障。
//
// 向量取自文档中的示例，PrivateKey 统一是 aliyuncdnexp1234。
// 期望摘要是**文档给出的值**，不是拿本实现反算的——
// 后者只能证明代码没变，证明不了代码是对的。
const vectorPrivateKey = "aliyuncdnexp1234"

// A 型：
//
//	URL        http://cdn.example.com/video/standard/1K.html
//	timestamp  1444435200   rand 0   uid 0
//	sstring    "/video/standard/1K.html-1444435200-0-0-aliyuncdnexp1234"
//	md5        80cd3862d699b7118eed99103f2a3a4f
func TestCDNAuthTypeAMatchesVendorVector(t *testing.T) {
	got := cdnAuthKeyA("/video/standard/1K.html", 1444435200, "0", "0", vectorPrivateKey)
	const want = "1444435200-0-0-80cd3862d699b7118eed99103f2a3a4f"
	if got != want {
		t.Fatalf("auth_key = %q, want %q", got, want)
	}
}

// B 型：
//
//	URL        http://cdn.example.com/4/44/44c0909bcfc20a01afaf256ca99a8b8b.mp3
//	timestamp  201508150800（即 2015-08-15 08:00 UTC）
//	sstring    "aliyuncdnexp1234201508150800/4/44/44c0909bcfc20a01afaf256ca99a8b8b.mp3"
//	md5        9044548ef1527deadafa49a890a377f0
func TestCDNAuthTypeBMatchesVendorVector(t *testing.T) {
	const uri = "/4/44/44c0909bcfc20a01afaf256ca99a8b8b.mp3"
	expireAt := time.Date(2015, 8, 15, 8, 0, 0, 0, time.UTC).Unix()
	got := cdnAuthPathB(uri, expireAt, vectorPrivateKey)
	const want = "/201508150800/9044548ef1527deadafa49a890a377f0" + uri
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// C 型：
//
//	URL        http://cdn.example.com/4/44/44c0909bcfc20a01afaf256ca99a8b8b.mp3
//	timestamp  0x55ce8100（即 1439596800）
//	sstring    "aliyuncdnexp1234/4/44/44c0909bcfc20a01afaf256ca99a8b8b.mp355ce8100"
//
// 与 A / B 不同：这里的期望摘要是**按文档给出的 sstring 拼法**
// 算出来的，不是文档里直接印着的数字（手头没有 C 型的示例摘要）。
// 它仍然钉住了最容易出错的东西——三段的**顺序**（key + uri + ts，
// 而不是 B 型的 key + ts + uri）。接入 C 型之前请对着当时的文档
// 再核一遍这个 sstring。
func TestCDNAuthTypeCFollowsDocumentedConcatenation(t *testing.T) {
	const uri = "/4/44/44c0909bcfc20a01afaf256ca99a8b8b.mp3"
	const expireAt int64 = 1439596800 // 0x55ce8100
	got := cdnAuthPathC(uri, expireAt, vectorPrivateKey)
	const want = "/d3b9f79df74a039d517a870307e7b122/55ce8100" + uri
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	// 顺序写反是这型最可能犯的错，单独钉一下：
	// B 是 key+ts+uri，C 是 key+uri+ts，两者只差一个位置。
	if strings.Contains(got, md5Sum(vectorPrivateKey+"55ce8100"+uri)) {
		t.Fatal("type C used type B's concatenation order (key+ts+uri)")
	}
}

// 三型的时间戳格式各不相同，混用是另一类静默失败：
// 格式对不上时边缘会把签名判成无效，而不会告诉你是时间格式的问题。
func TestCDNAuthTimestampFormatsDiffer(t *testing.T) {
	const uri = "/a/b.png"
	expireAt := time.Date(2026, 10, 23, 12, 34, 0, 0, time.UTC).Unix()

	a := cdnAuthKeyA(uri, expireAt, "0", "0", vectorPrivateKey)
	if !strings.HasPrefix(a, "1792758840-") {
		t.Errorf("type A should use a decimal unix timestamp, got %q", a)
	}
	b := cdnAuthPathB(uri, expireAt, vectorPrivateKey)
	if !strings.HasPrefix(b, "/202610231234/") {
		t.Errorf("type B should use yyyyMMddHHmm, got %q", b)
	}
	c := cdnAuthPathC(uri, expireAt, vectorPrivateKey)
	if !strings.Contains(c, "/6adb5438/") {
		t.Errorf("type C should use a hex timestamp, got %q", c)
	}
}

// A 型的 rand 必须是传进来的固定值而不是每次新随机数——
// 它是 URL 的一部分，变一次缓存键就变一次。
func TestCDNAuthTypeAIsStableAcrossCalls(t *testing.T) {
	const uri = "/a/b.png"
	first := cdnAuthKeyA(uri, 1444435200, cdnAuthRand, "0", vectorPrivateKey)
	second := cdnAuthKeyA(uri, 1444435200, cdnAuthRand, "0", vectorPrivateKey)
	if first != second {
		t.Fatalf("auth_key is not stable: %q vs %q", first, second)
	}
	if cdnAuthRand != "0" {
		t.Fatalf("cdnAuthRand = %q; a non-constant rand changes the cache key on every request", cdnAuthRand)
	}
}
