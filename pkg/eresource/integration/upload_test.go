package integration_test

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/local"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// 修复点 1：同名不同内容的文件不能互相覆盖。
func TestUploadSameNameDifferentContentDoesNotOverwrite(t *testing.T) {
	svc := newTestService(t, nil)
	first := upload(t, svc, "avatar.png", []byte("content-a"))
	second := upload(t, svc, "avatar.png", []byte("content-b"))

	if first.Resource.ObjectKey == second.Resource.ObjectKey {
		t.Fatalf("expected distinct object keys, both are %q", first.Resource.ObjectKey)
	}
	for _, res := range []*model.Resource{first.Resource, second.Resource} {
		// 文件名形如 {hash}-{random}.png：哈希保留下来方便运维对账，
		// 随机后缀保证每次上传都是独立对象。
		name := path.Base(res.ObjectKey)
		if !strings.HasPrefix(name, res.Hash+"-") || !strings.HasSuffix(name, ".png") {
			t.Errorf("object key %q should be named {hash}-{random}.png for hash %q", res.ObjectKey, res.Hash)
		}
		if res.Ext != ".png" {
			t.Errorf("ext = %q, want .png", res.Ext)
		}
	}
	assertFileContent(t, first.Resource.StorePath, "content-a")
	assertFileContent(t, second.Resource.StorePath, "content-b")
}

// 同内容重复上传必须落到不同的 object key。
//
// 早先的实现用纯内容哈希做文件名，于是两条 Resource 记录指向同一个物理对象，
// 删掉任意一条就会把文件删掉、让另一条永久 404。去重靠的是 Resource.Hash
// 字段而不是 object key，所以这里多占一份空间换来的是「一条记录 ↔ 一个对象」。
func TestIdenticalContentGetsDistinctObjectKeys(t *testing.T) {
	svc := newTestService(t, nil)
	first := upload(t, svc, "a.txt", []byte("same"))
	second := upload(t, svc, "b.txt", []byte("same"))
	if first.Resource.ObjectKey == second.Resource.ObjectKey {
		t.Fatalf("identical content must not share an object key, both are %q", first.Resource.ObjectKey)
	}
	if first.Resource.Hash != second.Resource.Hash {
		t.Fatalf("identical content should still share a hash, got %q and %q",
			first.Resource.Hash, second.Resource.Hash)
	}
	assertFileContent(t, first.Resource.StorePath, "same")
	assertFileContent(t, second.Resource.StorePath, "same")

	// 关键回归点：删掉其中一条，另一条必须还能读。
	if err := svc.Manager.Delete(context.Background(), first.Resource.ID, nil); err != nil {
		t.Fatalf("delete first: %v", err)
	}
	assertFileContent(t, second.Resource.StorePath, "same")
}

// 修复点 2：超出上限的上传必须被拒绝，且不能把整个请求体读进内存。
func TestUploadRejectsOversizedBody(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) { cfg.Manager.MaxFileSize = 16 })
	_, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "big.bin",
		Reader:   bytes.NewReader(bytes.Repeat([]byte("x"), 1024)),
	})
	if !errors.Is(err, manager.ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
}

// 未配置上限时也必须有兜底，不能无界读取。
func TestUploadHasDefaultSizeLimit(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) { cfg.Manager.MaxFileSize = 0 })
	body := bytes.Repeat([]byte("x"), int(manager.DefaultMaxFileSize)+1)
	_, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "huge.bin",
		Reader:   bytes.NewReader(body),
	})
	if !errors.Is(err, manager.ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
}

func TestUploadWhitelists(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.AllowedExts = []string{"png", ".jpg"}
		cfg.Manager.AllowedMimeTypes = []string{"image/png"}
	})
	if _, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "shell.sh", Reader: strings.NewReader("#!/bin/sh"),
	}); !errors.Is(err, manager.ErrExtNotAllowed) {
		t.Fatalf("err = %v, want ErrExtNotAllowed", err)
	}
	if _, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "a.png", ContentType: "text/html", Reader: strings.NewReader("<html>"),
	}); !errors.Is(err, manager.ErrMimeNotAllowed) {
		t.Fatalf("err = %v, want ErrMimeNotAllowed", err)
	}
	if _, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "a.PNG", ContentType: "image/png; charset=binary", Reader: bytes.NewReader(pngBytes()),
	}); err != nil {
		t.Fatalf("allowed upload rejected: %v", err)
	}
}

// 修复点 4：声明成图片、实则是 HTML 的内容必须被拦下（存储型 XSS 载体）。
func TestUploadRejectsMimeSpoofing(t *testing.T) {
	svc := newTestService(t, nil)
	_, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName:    "payload.png",
		ContentType: "image/png",
		Reader:      strings.NewReader("<html><script>alert(1)</script></html>"),
	})
	if !errors.Is(err, manager.ErrMimeMismatch) {
		t.Fatalf("err = %v, want ErrMimeMismatch", err)
	}

	// 真实 PNG 放行
	if _, err = svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "real.png", ContentType: "image/png", Reader: bytes.NewReader(pngBytes()),
	}); err != nil {
		t.Fatalf("real png rejected: %v", err)
	}

	// 非媒体类型不参与一致性校验（docx 会被嗅探成 application/zip）
	if _, err = svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName:    "a.docx",
		ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Reader:      bytes.NewReader([]byte("PK\x03\x04zip-ish-content")),
	}); err != nil {
		t.Fatalf("non-media upload rejected: %v", err)
	}

	// 需要上传 SVG 等格式时可显式关闭该校验。
	// 注意：关掉嗅探校验只是放行了「声明与内容不一致」，SVG 仍然过不了
	// public 策略的可执行内容拦截（见 TestPublicPolicyRejectsActiveContent），
	// 所以这里同时把它放到 private 策略下。
	lenient := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.SkipMimeSniffCheck = true
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
	})
	if _, err = lenient.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "icon.svg", ContentType: "image/svg+xml", Reader: strings.NewReader("<svg/>"),
	}); err != nil {
		t.Fatalf("lenient upload rejected: %v", err)
	}
}

// 修复点 6：去重不得跨业务、跨访问策略、跨上传人复用。
func TestDedupIsScoped(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.EnableDedup = true
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPublic
	})
	body := []byte("shared-content")
	base := func() *model.UploadRequest {
		return &model.UploadRequest{
			FileName: "f.txt", Reader: bytes.NewReader(body),
			BizType: "avatar", AccessPolicy: model.AccessPolicyPublic, Creator: "alice",
		}
	}
	first, err := svc.Manager.Upload(context.Background(), base())
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}

	// 完全同维度：命中去重
	same, err := svc.Manager.Upload(context.Background(), base())
	if err != nil {
		t.Fatalf("dedup upload: %v", err)
	}
	// new_record 策略下命中去重仍然新建一条记录：复用的是底层对象，
	// 不是那条历史记录，所以 ID 必须是新的、object key 必须相同。
	if !same.Existed || same.Uploaded {
		t.Errorf("identical scope should hit dedup, existed=%t uploaded=%t", same.Existed, same.Uploaded)
	}
	if same.Resource.ID == first.Resource.ID {
		t.Error("new_record dedup must create a new record")
	}
	if same.Resource.ObjectKey != first.Resource.ObjectKey {
		t.Error("new_record dedup must reuse the existing object")
	}

	// 换业务线：不得复用
	otherBiz := base()
	otherBiz.Reader = bytes.NewReader(body)
	otherBiz.BizType = "banner"
	got, err := svc.Manager.Upload(context.Background(), otherBiz)
	if err != nil {
		t.Fatalf("other biz upload: %v", err)
	}
	if got.Existed {
		t.Error("dedup must not cross biz_type")
	}

	// 换访问策略：private 上传不得拿到别人的 public 记录
	privateReq := base()
	privateReq.Reader = bytes.NewReader(body)
	privateReq.AccessPolicy = model.AccessPolicyPrivate
	got, err = svc.Manager.Upload(context.Background(), privateReq)
	if err != nil {
		t.Fatalf("private upload: %v", err)
	}
	if got.Existed {
		t.Error("dedup must not cross access policy")
	}

	// 非 public 场景换上传人：不得串号
	otherUser := base()
	otherUser.Reader = bytes.NewReader(body)
	otherUser.AccessPolicy = model.AccessPolicyPrivate
	otherUser.Creator = "bob"
	got, err = svc.Manager.Upload(context.Background(), otherUser)
	if err != nil {
		t.Fatalf("other user upload: %v", err)
	}
	if got.Existed {
		t.Error("dedup must not cross creator for non-public resources")
	}
}

// object key 不得逃出存储根目录。
func TestObjectKeyTraversalIsRejected(t *testing.T) {
	svc := newTestService(t, nil)
	if err := svc.Provider.Delete(context.Background(), "../../etc/passwd"); err == nil {
		t.Fatal("expected traversal object key to be rejected")
	}
	result := upload(t, svc, "x.txt", []byte("v"))
	if _, _, err := svc.Manager.OpenObject(context.Background(), "../"+result.Resource.ObjectKey); err == nil {
		t.Fatal("expected traversal on open to be rejected")
	}
}

// 非法身份字段应映射成 400 而不是 500。
func TestInvalidSegmentReturnsBadRequest(t *testing.T) {
	svc := newTestService(t, nil)
	handler, err := transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	t.Cleanup(handler.Close)
	router := http.NewServeMux()
	handler.Register(router)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(transport.FormFieldFile, "a.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(pngBytes()); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/resource/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	// 非法身份段必须从请求头进来：写在表单里的话命中的是「表单与请求头冲突」，
	// 400 看着一样，实际根本没走到段校验这一步。
	req.Header.Set(transport.HeaderAccountID, "a.b")
	if rec := serve(router, req); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// Provider 必须看到解析后的访问策略，而不是调用方留空的原值——
// OSS 要据此决定对象 ACL，看到空串就只能猜。
func TestProviderReceivesResolvedAccessPolicy(t *testing.T) {
	cases := []struct {
		name       string
		requested  string
		configured string
		want       string
	}{
		{"explicit public", model.AccessPolicyPublic, model.AccessPolicyPrivate, model.AccessPolicyPublic},
		{"explicit private", model.AccessPolicyPrivate, model.AccessPolicyPublic, model.AccessPolicyPrivate},
		{"falls back to config", "", model.AccessPolicyProtected, model.AccessPolicyProtected},
		{"config default public", "", model.AccessPolicyPublic, model.AccessPolicyPublic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			local, err := local.NewLocalProvider(config.ProviderConfig{
				RootDir: filepath.Join(dir, "objects"), Domain: "cdn.test", Scheme: "https",
			})
			if err != nil {
				t.Fatalf("new local provider: %v", err)
			}
			stub := &stubProvider{Provider: local}
			svc, err := eresource.New(config.Config{
				Store:    config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(dir, "meta.json")},
				Provider: config.ProviderConfig{Type: provider.TypeLocal, RootDir: filepath.Join(dir, "objects"), Domain: "cdn.test", Scheme: "https"},
				Manager:  config.ManagerConfig{DefaultAccessPolicy: tc.configured},
				Access:   config.AccessConfig{SignedURLSecret: "test-secret"},
			}, eresource.WithProvider(stub))
			if err != nil {
				t.Fatalf("new service: %v", err)
			}
			t.Cleanup(svc.Close)

			if _, err = svc.Manager.Upload(context.Background(), &model.UploadRequest{
				FileName: "a.txt", Reader: strings.NewReader("x"),
				BizType: "doc", Creator: "alice", AccessPolicy: tc.requested,
			}); err != nil {
				t.Fatalf("upload: %v", err)
			}
			if got := stub.policy(); got != tc.want {
				t.Errorf("provider saw access policy %q, want %q", got, tc.want)
			}
		})
	}
}

// 上传会把 MaxFileSize 以内的内容留在内存、超出部分落临时盘，两者都随并发数
// 线性增长。没有闸门时内存上界就是 MaxFileSize × 并发数，C 端放量足以打到 OOM。
func TestUploadConcurrencyGateRejectsOverflow(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.MaxConcurrentUploads = 1
	})
	router := newTestRouter(t, svc)

	buildBody := func() (*bytes.Buffer, string) {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		part, err := writer.CreateFormFile(transport.FormFieldFile, "note.txt")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err = part.Write([]byte("concurrent upload")); err != nil {
			t.Fatalf("write part: %v", err)
		}
		if err = writer.WriteField("biz_type", "doc"); err != nil {
			t.Fatalf("write field: %v", err)
		}
		if err = writer.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}
		return body, writer.FormDataContentType()
	}

	firstBody, contentType := buildBody()
	gated := &gatedReader{
		body:    bytes.NewReader(firstBody.Bytes()),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	firstReq := httptest.NewRequest(http.MethodPost, transport.RouteGroup+"/upload", gated)
	firstReq.Header.Set("Content-Type", contentType)

	firstDone := make(chan int, 1)
	go func() { firstDone <- serve(router, firstReq).Code }()

	<-gated.started // 第一个请求已经拿到槽位并开始读请求体

	secondBody, secondType := buildBody()
	secondReq := httptest.NewRequest(http.MethodPost, transport.RouteGroup+"/upload", secondBody)
	secondReq.Header.Set("Content-Type", secondType)
	secondRec := serve(router, secondReq)
	if secondRec.Code != http.StatusServiceUnavailable {
		t.Errorf("并发上传 status = %d，期望 503——并发闸门没拦住溢出的请求", secondRec.Code)
	}
	if retry := secondRec.Header().Get("Retry-After"); retry == "" {
		t.Error("503 响应缺少 Retry-After，调用方无法区分瞬时拥塞与永久失败")
	}

	close(gated.release)
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("第一个上传 status = %d，期望 200", code)
	}

	// 槽位释放之后必须能重新接收上传，否则闸门会把服务卡死
	thirdBody, thirdType := buildBody()
	thirdReq := httptest.NewRequest(http.MethodPost, transport.RouteGroup+"/upload", thirdBody)
	thirdReq.Header.Set("Content-Type", thirdType)
	if code := serve(router, thirdReq).Code; code != http.StatusOK {
		t.Fatalf("释放槽位后的上传 status = %d，期望 200——槽位没有被归还", code)
	}
}

// 上传响应和 Get / List 一样要脱敏：store_path / bucket 泄露出去，
// 等于把绕过验签所需的最后一块拼图给了对方。
func TestUploadResponseHidesStorageLayout(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)
	headers := map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-a",
		transport.HeaderUserID:    "alice",
	}

	rec := uploadVia(t, router, headers, map[string]string{"biz_type": "doc"}, "hide-layout")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "store_path") || strings.Contains(rec.Body.String(), `"bucket"`) {
		t.Errorf("上传响应泄露了底层存储布局：%s", rec.Body.String())
	}

	// 命中去重时返回的是历史记录，同样要脱敏
	rec = uploadVia(t, router, headers, map[string]string{"biz_type": "doc"}, "hide-layout")
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次 upload status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "store_path") || strings.Contains(rec.Body.String(), `"bucket"`) {
		t.Errorf("去重命中的响应泄露了底层存储布局：%s", rec.Body.String())
	}
}

// T3：public 资源的直链走 CDN / 静态目录，/content 那套 nosniff + CSP + attachment
// 防护完全不经过，所以可执行文档类型必须在入口就拦住。
func TestPublicPolicyRejectsActiveContent(t *testing.T) {
	// 关掉嗅探一致性校验，让 public 这道防线单独受测：
	// 否则「声明 image/svg+xml、内容是文本」会先被 ErrMimeMismatch 拦下，
	// 测不到真正想验证的东西。
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPublic
		cfg.Manager.SkipMimeSniffCheck = true
	})

	cases := []struct {
		name        string
		fileName    string
		contentType string
		body        string
	}{
		// 声明类型命中
		{"declared html", "payload.txt", "text/html", "<html>x</html>"},
		{"declared svg", "icon.bin", "image/svg+xml", "<svg/>"},
		// 扩展名命中：声明成 octet-stream，但 nginx / OSS 会按 .html 回 text/html
		{"html ext", "payload.html", "application/octet-stream", "plain text"},
		{"svg ext", "icon.svg", "application/octet-stream", "plain text"},
		// 嗅探命中：不声明类型、也没有后缀
		{"sniffed html", "payload", "", "<html><body>x</body></html>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
				FileName: tc.fileName, ContentType: tc.contentType, Reader: strings.NewReader(tc.body),
			})
			if !errors.Is(err, manager.ErrUnsafePublicContent) {
				t.Fatalf("err = %v, want ErrUnsafePublicContent", err)
			}
		})
	}

	// 受控策略不受影响：这条路径由 /content 的 CSP + attachment 兜底
	controlled := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPrivate
	})
	if _, err := controlled.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "page.html", ContentType: "text/html", Reader: strings.NewReader("<html/>"),
	}); err != nil {
		t.Fatalf("private 策略下的 HTML 被拒：%v", err)
	}

	// 确有需求时可以显式放开
	opted := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPublic
		cfg.Manager.AllowPublicActiveContent = true
		cfg.Manager.SkipMimeSniffCheck = true
	})
	if _, err := opted.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "icon.svg", ContentType: "image/svg+xml", Reader: strings.NewReader("<svg/>"),
	}); err != nil {
		t.Fatalf("显式放开后仍被拒：%v", err)
	}

	// 普通图片不受影响
	if _, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "real.png", ContentType: "image/png", Reader: bytes.NewReader(pngBytes()),
	}); err != nil {
		t.Fatalf("正常图片被误伤：%v", err)
	}
}

// HTTP 层把这类拒绝映射成 415，而不是含混的 500。
func TestPublicActiveContentOverHTTPIs415(t *testing.T) {
	svc := newTestService(t, nil)
	router := newTestRouter(t, svc)
	rec := uploadVia(t, router, map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-a",
		transport.HeaderUserID:    "alice",
	}, map[string]string{"biz_type": "doc"}, "<html><script>alert(1)</script></html>")
	// note.txt + 纯文本声明，嗅探结果为 text/html
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415, body = %s", rec.Code, rec.Body.String())
	}
}

// T1：上传必须脱离 ehttp 的 3 秒请求超时，否则大文件会在推给存储的途中
// 被掐断，留下一个已写入存储却没有元数据的孤儿对象。
func TestUploadDetachesFromRequestTimeout(t *testing.T) {
	svc, p := newDirectService(t, func(cfg *config.Config) {
		cfg.Manager.UploadTimeoutSeconds = 120
	})
	router := newTestRouter(t, svc)
	rec := uploadVia(t, router, map[string]string{
		transport.HeaderNamespace: "crm",
		transport.HeaderAccountID: "acct-a",
		transport.HeaderUserID:    "alice",
	}, map[string]string{"biz_type": "doc"}, "detached")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	p.mu.Lock()
	budget, hadLimit := p.uploadBudget, p.uploadHadLimit
	p.mu.Unlock()
	if !hadLimit {
		t.Fatal("上传 ctx 没有任何超时，上限必须存在——否则一次卡住的上传会永远占着并发槽位")
	}
	// ehttp 的单请求预算是 3 秒，这里应当看到 upload_timeout_seconds 那一档。
	if budget < 60*time.Second {
		t.Errorf("上传 ctx 剩余预算 = %v，仍然是请求级超时；期望按 upload_timeout_seconds 重新计时", budget)
	}
}

// 去重命中时底层对象可能已被外部清理过：元数据还在，对象没了。
// 不核一下就会返回一条指向 404 的记录，而调用方刚刚明明「上传成功」了。
func TestDedupIgnoresRecordsWhoseObjectIsGone(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.EnableDedup = true
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
	})
	first := uploadSharedContent(t, svc, "a.png")

	// 模拟外部手工清理：对象没了，元数据还在。
	if err := os.Remove(first.StorePath); err != nil {
		t.Fatalf("remove object: %v", err)
	}

	second, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "b.png", Reader: strings.NewReader(sharedBody),
		Namespace: "crm", AccountID: "acct-a", BizType: "doc",
		AccessPolicy: model.AccessPolicyProtected, Creator: "alice",
	})
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if second.Existed || !second.Uploaded {
		t.Fatalf("stale dedup target must not be reused, existed=%t uploaded=%t",
			second.Existed, second.Uploaded)
	}
	// 新对象必须真的落盘了。
	assertFileContent(t, second.Resource.StorePath, sharedBody)
}

// 版本管理不实现，但字段要有确定的起点，将来接版本链不必回填存量数据。
func TestUploadStampsInitialVersion(t *testing.T) {
	svc := newTestService(t, nil)
	result := upload(t, svc, "v1.png", pngBytes())
	if result.Resource.Version != 1 {
		t.Errorf("version = %d, want 1", result.Resource.Version)
	}
	if result.Resource.ParentID != "" {
		t.Errorf("parent_id = %q, want empty for a first version", result.Resource.ParentID)
	}
}

func TestContentInspectorRollsBackRejectedUpload(t *testing.T) {
	inspector := &rejectingInspector{}
	svc := newInspectedService(t, inspector)
	ctx := context.Background()

	_, err := svc.Manager.Upload(ctx, &model.UploadRequest{
		FileName: "bad.txt",
		Reader:   bytes.NewReader([]byte("forbidden content")),
		BizType:  "ugc",
	})
	if !errors.Is(err, manager.ErrContentRejected) {
		t.Fatalf("err = %v, want ErrContentRejected", err)
	}
	// 违规内容不该留下任何可访问的痕迹。
	total, err := svc.Manager.Count(ctx, &model.Query{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 0 {
		t.Fatalf("a rejected upload left %d records behind", total)
	}
}

func TestContentInspectorAllowsCleanUpload(t *testing.T) {
	inspector := &rejectingInspector{}
	svc := newInspectedService(t, inspector)
	result := upload(t, svc, "good.png", pngBytes())
	if result.Resource.ID == "" {
		t.Fatal("clean upload should succeed")
	}
	inspector.mu.Lock()
	defer inspector.mu.Unlock()
	if len(inspector.seen) != 1 {
		t.Fatalf("inspector saw %d uploads, want 1", len(inspector.seen))
	}
}

// gatedReader 在第一次被读取时发信号，随后阻塞，直到 release 被关闭。
// 用来把一个上传请求「钉」在处理中，好观察第二个请求撞上并发闸门。
type gatedReader struct {
	body    *bytes.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedReader) Read(p []byte) (int, error) {
	g.once.Do(func() {
		close(g.started)
		<-g.release
	})
	return g.body.Read(p)
}

type rejectingInspector struct {
	mu   sync.Mutex
	seen []string
}

func (r *rejectingInspector) Inspect(_ context.Context, resource *model.Resource, body []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, resource.ID)
	if bytes.Contains(body, []byte("forbidden")) {
		return manager.ErrContentRejected
	}
	return nil
}

func newInspectedService(t *testing.T, inspector manager.ContentInspector) *eresource.Service {
	t.Helper()
	svc, err := eresource.New(baseConfig(t.TempDir()), eresource.WithContentInspector(inspector))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}
