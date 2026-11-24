package integration_test

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// stubProvider 是一个可观测的 Provider：记录 Upload 收到的请求，
// 并让 Open 返回一个会在读取时检查 ctx 的 reader。
type stubProvider struct {
	provider.Provider
	mu         sync.Mutex
	lastUpload model.UploadRequest
	readDelay  time.Duration
	readCtxErr error
}

func (s *stubProvider) Upload(ctx context.Context, req *model.UploadRequest) (*model.ProviderResult, error) {
	s.mu.Lock()
	s.lastUpload = *req
	s.mu.Unlock()
	return s.Provider.Upload(ctx, req)
}

func (s *stubProvider) policy() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUpload.AccessPolicy
}

func (s *stubProvider) Open(ctx context.Context, objectKey string) (io.ReadCloser, *model.ObjectStat, error) {
	inner, stat, err := s.Provider.(provider.ObjectReader).Open(ctx, objectKey)
	if err != nil {
		return nil, nil, err
	}
	return &stubReader{parent: s, ctx: ctx, inner: inner}, stat, nil
}

type stubReader struct {
	parent *stubProvider
	ctx    context.Context
	inner  io.ReadCloser
	probed bool
}

func (r *stubReader) Read(p []byte) (int, error) {
	if !r.probed {
		r.probed = true
		// 模拟一次「比框架单请求超时更久」的读取
		time.Sleep(r.parent.readDelay)
		r.parent.mu.Lock()
		r.parent.readCtxErr = r.ctx.Err()
		r.parent.mu.Unlock()
	}
	return r.inner.Read(p)
}

func (r *stubReader) Close() error { return r.inner.Close() }

// memProvider 是一个支持直传的内存 Provider。
//
// local 刻意不实现 DirectUploader（本地盘没有客户端可直连的端点），
// 因此直传相关的测试必须自带一个实现。它同时记录 Upload 拿到的 ctx 预算，
// 用于验证上传路径确实脱离了框架的 3 秒请求超时。
type memProvider struct {
	mu             sync.Mutex
	objects        map[string][]byte
	acl            map[string]string
	uploadBudget   time.Duration
	uploadHadLimit bool
	signedExpire   int64
}

func newMemProvider() *memProvider {
	return &memProvider{objects: map[string][]byte{}, acl: map[string]string{}}
}

// SetObjectACL 让这个 stub 同时具备 provider.ACLSetter 能力，
// 用来观察访问策略变更有没有真的落到底层对象上。
func (p *memProvider) SetObjectACL(_ context.Context, objectKey, accessPolicy string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acl[objectKey] = accessPolicy
	return nil
}

func (p *memProvider) aclOf(objectKey string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acl[objectKey]
}

func (p *memProvider) Name() string { return "mem" }

func (p *memProvider) Upload(ctx context.Context, req *model.UploadRequest) (*model.ProviderResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		p.uploadHadLimit = true
		p.uploadBudget = time.Until(deadline)
	}
	key := "mem/" + req.Hash + req.Ext
	p.objects[key] = append([]byte(nil), req.Bytes...)
	p.acl[key] = req.AccessPolicy
	return &model.ProviderResult{
		Provider: p.Name(), Bucket: "mem-bucket", ObjectKey: key,
		StorePath: "mem-bucket/" + key, Size: int64(len(req.Bytes)),
	}, nil
}

func (p *memProvider) Delete(_ context.Context, objectKey string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.objects, objectKey)
	return nil
}

func (p *memProvider) Stat(_ context.Context, objectKey string) (*model.ObjectStat, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	body, ok := p.objects[objectKey]
	if !ok {
		return &model.ObjectStat{Exists: false}, nil
	}
	return &model.ObjectStat{Exists: true, Size: int64(len(body)), ContentType: "application/octet-stream"}, nil
}

func (p *memProvider) BuildURL(objectKey string, _ *model.URLBuildOption) (string, error) {
	return "https://cdn.test/" + objectKey, nil
}

func (p *memProvider) BuildSignedURL(_ context.Context, objectKey string, opts *model.SignedURLBuildOption) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if opts != nil {
		p.signedExpire = opts.ExpireSeconds
	}
	return "https://cdn.test/" + objectKey + "?signed=1", nil
}

// SignUploadPolicy 模拟对象存储签发的表单凭证。
func (p *memProvider) SignUploadPolicy(_ context.Context, req *model.DirectUploadRequest) (*model.DirectUploadPolicy, error) {
	key := "mem/direct/" + req.Namespace + "/" + req.AccountID + "/obj" + req.Ext
	return &model.DirectUploadPolicy{
		Provider: p.Name(), Method: http.MethodPost, UploadURL: "https://mem-bucket.test/",
		FileField: "file",
		FormFields: map[string]string{
			"key": key, "Content-Type": req.ContentType, "policy": "stub-policy",
		},
		ObjectKey: key, MaxSize: req.MaxSize,
		ExpireAt:  time.Now().Add(time.Duration(req.ExpireSeconds) * time.Second),
		Bucket:    "mem-bucket",
		StorePath: "mem-bucket/" + key,
	}, nil
}

// putDirect 模拟客户端把文件直传到了对象存储。
func (p *memProvider) putDirect(objectKey string, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.objects[objectKey] = append([]byte(nil), body...)
}
