//go:build eresource_oss_integration

package aliyunoss

// 阿里云 OSS 真机联调。使用专用测试 bucket，测试会创建并删除对象。
// 凭据从 ERESOURCE_OSS_BUCKET、ERESOURCE_OSS_ENDPOINT、ERESOURCE_OSS_AK 和
// ERESOURCE_OSS_SK 读取。Region 和 Domain 可通过同前缀的环境变量覆盖。

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

var ossManualConfig = config.ProviderConfig{
	Type:     provider.TypeAliyunSS,
	Scheme:   "https",
	BasePath: "eresource-manual",
}

// ossEnv 环境变量覆盖，优先级高于上面的常量。
//
//	ERESOURCE_OSS_BUCKET / _ENDPOINT / _REGION / _AK / _SK / _DOMAIN
func ossEnv(cfg config.ProviderConfig) config.ProviderConfig {
	for _, binding := range []struct {
		env   string
		field *string
	}{
		{"ERESOURCE_OSS_BUCKET", &cfg.Bucket},
		{"ERESOURCE_OSS_ENDPOINT", &cfg.Endpoint},
		{"ERESOURCE_OSS_REGION", &cfg.Region},
		{"ERESOURCE_OSS_AK", &cfg.AccessKey},
		{"ERESOURCE_OSS_SK", &cfg.SecretKey},
		{"ERESOURCE_OSS_DOMAIN", &cfg.Domain},
	} {
		if value := strings.TrimSpace(os.Getenv(binding.env)); value != "" {
			*binding.field = value
		}
	}
	return cfg
}

// largeObjectSize 用于验证「读流不被截断」的对象大小。
// 要明显大于一次 TCP 窗口，太小的话即使实现有 bug 也可能一次读完看不出来。
const largeObjectSize = 8 << 20 // 8MB

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

type ossManualFixture struct {
	t        *testing.T
	provider *AliOSSProvider
	bucket   *oss.Bucket

	mu   sync.Mutex
	keys []string // 需要在收尾时删除的对象
}

func newOSSManualFixture(t *testing.T) *ossManualFixture {
	t.Helper()
	cfg := ossEnv(ossManualConfig)

	var missing []string
	for _, required := range []struct {
		name  string
		value string
	}{
		{"Bucket", cfg.Bucket},
		{"Endpoint", cfg.Endpoint},
		{"AccessKey", cfg.AccessKey},
		{"SecretKey", cfg.SecretKey},
	} {
		if strings.TrimSpace(required.value) == "" {
			missing = append(missing, required.name)
		}
	}
	if len(missing) > 0 {
		t.Skipf("OSS 联调用例已跳过：缺少 %s。\n"+
			"    设置 ERESOURCE_OSS_BUCKET / _ENDPOINT / _AK / _SK 环境变量后重试。",
			strings.Join(missing, ", "))
	}

	p, err := NewAliOSSProvider(cfg)
	if err != nil {
		t.Fatalf("构造 Provider 失败：%v\n"+
			"    region 为空时会从 endpoint 推断，推断不出就会在这里报错——"+
			"显式填上 Region 再试。", err)
	}
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		t.Fatalf("获取 bucket 句柄失败：%v", err)
	}

	f := &ossManualFixture{t: t, provider: p, bucket: bucket}
	t.Cleanup(f.cleanup)

	t.Logf("配置就绪：bucket=%s endpoint=%s region=%s cname=%t domain=%q",
		p.bucketName, p.endpoint, p.region, p.useCname, p.domain)
	return f
}

// cleanup 删除本次跑出来的所有对象。
func (f *ossManualFixture) cleanup() {
	f.mu.Lock()
	keys := append([]string(nil), f.keys...)
	f.mu.Unlock()
	for _, key := range keys {
		if err := f.bucket.DeleteObject(key); err != nil {
			f.t.Logf("清理对象 %s 失败（可手工删除 %s/ 前缀）：%v",
				key, f.provider.basePath, err)
		}
	}
}

func (f *ossManualFixture) track(objectKey string) {
	f.mu.Lock()
	f.keys = append(f.keys, objectKey)
	f.mu.Unlock()
}

// upload 按 Manager 的方式准备好请求再交给 Provider：
// Hash 与 Ext 本来就是 Manager 回填的，这里手工补上以复现真实调用形态。
// tryUpload 上传并返回错误，供需要区分失败原因的子测试使用。
func (f *ossManualFixture) tryUpload(policy, ext string, body []byte) (*model.ProviderResult, error) {
	sum := md5.Sum(body)
	req := &model.UploadRequest{
		FileName:     "manual" + ext,
		ContentType:  "application/octet-stream",
		Bytes:        body,
		Namespace:    "manual",
		AccountID:    "acct1",
		BizType:      "verify",
		AccessPolicy: policy,
		Creator:      "manual-test",
		Hash:         hex.EncodeToString(sum[:]),
		Ext:          ext,
	}
	result, err := f.provider.Upload(context.Background(), req)
	if err != nil {
		return nil, err
	}
	f.track(result.ObjectKey)
	return result, nil
}

func (f *ossManualFixture) upload(t *testing.T, policy, ext string, body []byte) *model.ProviderResult {
	t.Helper()
	result, err := f.tryUpload(policy, ext, body)
	if err != nil {
		t.Fatalf("上传 %s 对象失败：%v", policy, err)
	}
	return result
}

// httpGet 发一次普通 HTTP GET，返回状态码与响应体。
func httpGet(t *testing.T, rawURL string) (int, []byte) {
	t.Helper()
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("请求 %s 失败：%v\n"+
			"    若配置了自定义 Domain，请先确认该域名已在 OSS 侧绑定且 DNS 已生效。",
			rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s 响应体失败：%v", rawURL, err)
	}
	return resp.StatusCode, body
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestAliOSSManualVerification 按上线检查单的顺序逐项验证 OSS 链路。
//
// 子用例之间没有依赖，可以用 -run 单独跑其中一条，例如：
//
//	go test -tags eresource_oss_integration -v -run 'TestAliOSSManual/签名地址' ./pkg/eresource/provider/aliyunoss
func TestAliOSSManualVerification(t *testing.T) {
	f := newOSSManualFixture(t)
	payload := []byte("eresource manual verification " + time.Now().Format(time.RFC3339Nano))

	t.Run("对象Key带租户前缀", func(t *testing.T) {
		result := f.upload(t, model.AccessPolicyPrivate, ".bin", payload)
		t.Logf("object key = %s", result.ObjectKey)

		wantPrefix := "eresource-manual/manual/acct1/verify/"
		if f.provider.basePath == "" {
			wantPrefix = "manual/acct1/verify/"
		}
		if !strings.HasPrefix(result.ObjectKey, wantPrefix) {
			t.Errorf("object key 前缀 = %q，期望以 %q 开头", result.ObjectKey, wantPrefix)
		}
		// 形如 {md5}-{random}.bin：哈希便于对账，随机后缀保证一条记录一个对象
		name := result.ObjectKey[strings.LastIndex(result.ObjectKey, "/")+1:]
		if !strings.HasSuffix(name, ".bin") || !strings.Contains(name, "-") {
			t.Errorf("文件名 = %q，期望 {md5}-{random}.bin 形态", name)
		}
		t.Log("✅ 对象 Key 的租户前缀与唯一后缀符合预期")
	})

	t.Run("public对象ACL与直链", func(t *testing.T) {
		result, err := f.tryUpload(model.AccessPolicyPublic, ".txt", payload)
		if err != nil {
			// bucket 开了「阻止公共访问」时，带 public-read ACL 的 PutObject 会被
			// 直接拒绝。这是 bucket 策略覆盖对象 ACL 的正常表现，不是代码缺陷，
			// 跳过而不是失败——要验证这一段，先在控制台关掉该 bucket 的阻止公共访问。
			if strings.Contains(err.Error(), "Put public object acl is not allowed") {
				t.Skipf("bucket 禁止公共访问，跳过 public 直链验证（这是 bucket 策略生效，不是缺陷）：%v", err)
			}
			t.Fatalf("上传 public 对象失败：%v", err)
		}

		acl, err := f.bucket.GetObjectACL(result.ObjectKey)
		if err != nil {
			t.Fatalf("查询对象 ACL 失败：%v", err)
		}
		t.Logf("public 对象 ACL = %s", acl.ACL)
		if acl.ACL != string(oss.ACLPublicRead) {
			t.Errorf("public 对象 ACL = %q，期望 %q\n"+
				"    说明 Upload 没有正确下传 AccessPolicy，或 bucket 策略覆盖了对象 ACL。",
				acl.ACL, oss.ACLPublicRead)
		}

		directURL, err := f.provider.BuildURL(result.ObjectKey, nil)
		if err != nil {
			t.Fatalf("构建直链失败：%v", err)
		}
		t.Logf("public 直链 = %s", directURL)
		status, body := httpGet(t, directURL)
		if status != http.StatusOK {
			t.Errorf("public 直链状态码 = %d，期望 200\n"+
				"    若为 403：检查 bucket 是否开启了「阻止公共访问」——"+
				"该开关会让对象级 public-read 失效。", status)
		} else if string(body) != string(payload) {
			t.Errorf("public 直链内容与上传内容不一致")
		} else {
			t.Log("✅ public 对象 ACL 正确，直链可匿名访问且内容一致")
		}
	})

	t.Run("private对象不可匿名访问", func(t *testing.T) {
		result := f.upload(t, model.AccessPolicyPrivate, ".txt", payload)

		acl, err := f.bucket.GetObjectACL(result.ObjectKey)
		if err != nil {
			t.Fatalf("查询对象 ACL 失败：%v", err)
		}
		t.Logf("private 对象 ACL = %s", acl.ACL)
		if acl.ACL != string(oss.ACLPrivate) {
			t.Errorf("private 对象 ACL = %q，期望 %q —— 这是越权泄露，必须修复",
				acl.ACL, oss.ACLPrivate)
		}

		directURL, err := f.provider.BuildURL(result.ObjectKey, nil)
		if err != nil {
			t.Fatalf("构建直链失败：%v", err)
		}
		status, _ := httpGet(t, directURL)
		t.Logf("private 裸直链状态码 = %d", status)
		if status == http.StatusOK {
			t.Errorf("private 对象可以被匿名直接下载（状态码 200）——"+
				"签名体系被绕过。检查 bucket ACL 是否为 public-read。url=%s", directURL)
		} else {
			t.Log("✅ private 对象拒绝匿名访问")
		}
	})

	t.Run("签名地址", func(t *testing.T) {
		result := f.upload(t, model.AccessPolicyPrivate, ".txt", payload)

		signed, err := f.provider.BuildSignedURL(context.Background(), result.ObjectKey,
			&model.SignedURLBuildOption{ExpireSeconds: 300})
		if err != nil {
			t.Fatalf("生成签名地址失败：%v", err)
		}
		t.Logf("签名地址 = %s", signed)

		status, body := httpGet(t, signed)
		if status != http.StatusOK {
			t.Errorf("签名地址状态码 = %d，期望 200\n"+
				"    403 SignatureDoesNotMatch 的典型原因：\n"+
				"      a) region 与 bucket 实际地域不符；\n"+
				"      b) 配了自定义 Domain 但没在 OSS 侧绑定该 CNAME；\n"+
				"      c) 签完之后 URL 的 Host 被改写过（V4 把 Host 算进签名）。",
				status)
		} else if string(body) != string(payload) {
			t.Errorf("签名地址返回的内容与上传内容不一致")
		} else {
			t.Log("✅ V4 签名地址可访问且内容一致（含 CNAME 模式）")
		}
	})

	t.Run("签名过期后失效", func(t *testing.T) {
		result := f.upload(t, model.AccessPolicyPrivate, ".txt", payload)

		signed, err := f.provider.BuildSignedURL(context.Background(), result.ObjectKey,
			&model.SignedURLBuildOption{ExpireSeconds: 1})
		if err != nil {
			t.Fatalf("生成短效签名失败：%v", err)
		}
		time.Sleep(3 * time.Second)
		status, _ := httpGet(t, signed)
		t.Logf("过期签名状态码 = %d", status)
		if status == http.StatusOK {
			t.Error("过期的签名地址仍然可以访问——时效控制没有生效")
		} else {
			t.Log("✅ 过期签名已失效")
		}
	})

	t.Run("服务端读流不截断", func(t *testing.T) {
		large := make([]byte, largeObjectSize)
		for i := range large {
			large[i] = byte(i % 251) // 非全零，避免被任何一层稀疏优化掩盖问题
		}
		result := f.upload(t, model.AccessPolicyPrivate, ".bin", large)
		t.Logf("已上传 %d 字节，开始服务端读流", len(large))

		start := time.Now()
		reader, stat, err := f.provider.Open(context.Background(), result.ObjectKey)
		if err != nil {
			t.Fatalf("Open 失败：%v", err)
		}
		defer reader.Close()

		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("读流失败：%v\n"+
				"    若错误里带 context canceled / deadline exceeded，"+
				"说明下载沿用了上游的短超时——参见 access.download_timeout_seconds。", err)
		}
		t.Logf("读取完成：%d 字节，耗时 %s", len(got), time.Since(start).Round(time.Millisecond))

		if len(got) != len(large) {
			t.Errorf("读到 %d 字节，期望 %d —— 内容被截断", len(got), len(large))
		} else if string(got) != string(large) {
			t.Error("读到的内容与上传内容不一致")
		} else {
			t.Log("✅ 大对象服务端读流完整，无截断")
		}
		if stat != nil {
			t.Logf("ObjectStat: size=%d content_type=%s updated_at=%s",
				stat.Size, stat.ContentType, stat.UpdatedAt.Format(time.RFC3339))
			if stat.Size != int64(len(large)) {
				t.Errorf("stat.Size = %d，期望 %d", stat.Size, len(large))
			}
		}
	})

	t.Run("Stat与不存在对象", func(t *testing.T) {
		result := f.upload(t, model.AccessPolicyPrivate, ".txt", payload)

		stat, err := f.provider.Stat(context.Background(), result.ObjectKey)
		if err != nil {
			t.Fatalf("Stat 已存在对象失败：%v", err)
		}
		if !stat.Exists || stat.Size != int64(len(payload)) {
			t.Errorf("Stat = %+v，期望 exists=true size=%d", stat, len(payload))
		}

		missing := result.ObjectKey + ".does-not-exist"
		stat, err = f.provider.Stat(context.Background(), missing)
		if err != nil {
			t.Errorf("Stat 不存在的对象返回了错误 %v，期望 exists=false 且无错误", err)
		} else if stat.Exists {
			t.Error("Stat 把不存在的对象判成了存在")
		}

		_, _, err = f.provider.Open(context.Background(), missing)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open 不存在的对象返回 %v，期望可被 errors.Is(err, fs.ErrNotExist) 命中\n"+
				"    transport 层靠这个哨兵把响应映射成 404，映射不上就会变成 500。", err)
		} else {
			t.Log("✅ 404 判定正确（Stat 不报错、Open 返回 fs.ErrNotExist）")
		}
	})

	t.Run("删除", func(t *testing.T) {
		result := f.upload(t, model.AccessPolicyPrivate, ".txt", payload)

		if err := f.provider.Delete(context.Background(), result.ObjectKey); err != nil {
			t.Fatalf("删除失败：%v", err)
		}
		stat, err := f.provider.Stat(context.Background(), result.ObjectKey)
		if err != nil {
			t.Fatalf("删除后 Stat 失败：%v", err)
		}
		if stat.Exists {
			t.Error("删除后对象仍然存在")
		}
		// 重复删除必须幂等，否则元数据会永远删不掉
		if err := f.provider.Delete(context.Background(), result.ObjectKey); err != nil {
			t.Errorf("重复删除返回错误 %v，期望幂等成功", err)
		}
		t.Log("✅ 删除生效且可重复执行")
	})
}

// TestAliOSSManualRegionInference 单独验证 region 推断结果与真实 bucket 是否吻合。
//
// 推断错了的表现是签名 403，排查起来很绕，所以单独拎出来早点暴露。
func TestAliOSSManualRegionInference(t *testing.T) {
	cfg := ossEnv(ossManualConfig)
	if strings.TrimSpace(cfg.Endpoint) == "" {
		t.Skip("OSS 联调用例已跳过：未配置 Endpoint")
	}
	inferred := extractRegion(cfg.Endpoint)
	t.Logf("endpoint=%s → 推断 region=%q", cfg.Endpoint, inferred)
	if inferred == "" {
		t.Logf("⚠️  推断不出 region，必须在配置里显式指定 Provider.Region")
		return
	}
	if explicit := strings.TrimSpace(cfg.Region); explicit != "" && explicit != inferred {
		t.Errorf("显式配置的 region %q 与推断值 %q 不一致，请确认哪个才是 bucket 的真实地域",
			explicit, inferred)
		return
	}
	t.Logf("✅ region 推断结果可用于 V4 签名（最终以「签名地址」用例的实际结果为准）")
}
