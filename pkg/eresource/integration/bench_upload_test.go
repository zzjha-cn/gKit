package integration_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

// BenchmarkUploadMemory 测量单次上传的内存放大（T11）。
//
// 背景：`ParseMultipartForm(maxMemory)` 会把 maxMemory 以内的文件整份留在
// 内存里，而 `readUploadBody` 随后还要 ReadAll 一份。把 max_file_size 当作
// maxMemory 传进去时，单次上传的峰值是文件大小的两倍。
//
//	go test -run XXX -bench BenchmarkUploadMemory -benchmem -benchtime 10x .
//
// 8MB 文件（darwin/arm64，本地盘 + JSONStore）：
//
//	maxMemory = 32MB（修改前）   33.5 ms/op   50.7 MB/op
//	maxMemory =  1MB（修改后）   51.6 ms/op   21.4 MB/op
//
// 512KB 文件两种配置下没有差别（~7.2 ms/op、~3.2 MB/op）：
// 小于 maxMemory 的上传本来就不落盘，阈值调小只影响大文件。
// 换来的是确定的磁盘 I/O，换掉的是不确定的 OOM。
func BenchmarkUploadMemory(b *testing.B) {
	dir := b.TempDir()
	svc, err := eresource.New(config.Config{
		Manager: config.ManagerConfig{DefaultAccessPolicy: model.AccessPolicyPublic, MaxFileSize: 32 << 20},
		Store:   config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(dir, "meta.json")},
		Provider: config.ProviderConfig{
			Type: provider.TypeLocal, RootDir: filepath.Join(dir, "objects"),
			Domain: "cdn.test", Scheme: "https",
		},
		Access: config.AccessConfig{SignedURLSecret: "bench", SignedURLDefaultExpire: 60},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer svc.Close()

	handler, err := transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config)
	if err != nil {
		b.Fatal(err)
	}
	defer handler.Close()
	router := http.NewServeMux()
	handler.Register(router)

	// 请求体只构造一次：把 multipart 组装算进来的话，
	// 测到的是测试代码自己的分配，而不是上传路径的。
	raw, contentType := benchUploadBody(b, bytes.Repeat([]byte("x"), 8<<20))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, transport.RouteGroup+"/upload", bytes.NewReader(raw))
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	}
}

func benchUploadBody(b *testing.B, payload []byte) ([]byte, string) {
	b.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(transport.FormFieldFile, "blob.bin")
	if err != nil {
		b.Fatal(err)
	}
	if _, err = part.Write(payload); err != nil {
		b.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		b.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}
