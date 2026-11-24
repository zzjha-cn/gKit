package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/transport"
)

func TestHTTPEndToEnd(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
	})
	handler, err := transport.NewHTTPHandler(svc.Manager, svc.Provider, svc.Config)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	router := http.NewServeMux()
	handler.Register(router)

	// 上传
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(transport.FormFieldFile, "note.txt")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	part.Write([]byte("hello eresource"))
	writer.WriteField("biz_type", "doc")
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, transport.RouteGroup+"/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := serve(router, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var uploaded struct {
		Data model.UploadResult `json:"data"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	id := uploaded.Data.Resource.ID
	objectKey := uploaded.Data.Resource.ObjectKey
	if id == "" {
		t.Fatal("empty resource id")
	}

	// 查询
	rec = serve(router, httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 不存在的 ID 必须是 404 而不是 500
	rec = serve(router, httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing resource status = %d, want 404", rec.Code)
	}

	// 取签名地址
	accessReq := httptest.NewRequest(http.MethodGet, transport.RouteGroup+"/"+id+"/access", nil)
	accessReq.Header.Set("X-User-Id", "u1")
	rec = serve(router, accessReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("access status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var access struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &access); err != nil {
		t.Fatalf("decode access response: %v", err)
	}
	signedURL, err := url.Parse(access.Data.URL)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}

	// 带合法签名下载
	rec = serve(router, httptest.NewRequest(http.MethodGet, signedURL.Path+"?"+signedURL.RawQuery, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("signed download status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello eresource" {
		t.Errorf("downloaded body = %q", got)
	}

	// 去掉签名参数后必须被拒绝
	rec = serve(router, httptest.NewRequest(http.MethodGet, signedURL.Path, nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("unsigned download status = %d, want 403", rec.Code)
	}

	// 篡改签名后必须被拒绝
	tampered := signedURL.Query()
	tampered.Set(provider.SignParamSign, strings.Repeat("0", 64))
	rec = serve(router, httptest.NewRequest(http.MethodGet, signedURL.Path+"?"+tampered.Encode(), nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("tampered download status = %d, want 403", rec.Code)
	}

	// 删除
	rec = serve(router, httptest.NewRequest(http.MethodDelete, transport.RouteGroup+"/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, _, err = svc.Manager.OpenObject(context.Background(), objectKey); err == nil {
		t.Error("object should be gone after delete")
	}
}
