package local

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/internal/object"
)

const LocalProviderName = provider.TypeLocal

type LocalProvider struct {
	rootDir        string
	scheme         string
	host           string
	basePath       string
	signedBasePath string
}

func NewLocalProvider(cfg config.ProviderConfig) (*LocalProvider, error) {
	if cfg.RootDir == "" {
		return nil, fmt.Errorf("provider root dir is empty")
	}
	rootDir, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		return nil, err
	}
	// 与 config.Validate 一致：Domain 不给默认值。直接调用本构造函数（绕过
	// eresource.New）的场景同样不能静默落到 localhost——public 直链会被持久化。
	if strings.TrimSpace(cfg.Domain) == "" {
		return nil, fmt.Errorf("provider domain is empty")
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "http"
	}
	if cfg.SignedBasePath == "" {
		cfg.SignedBasePath = config.DefaultSignedBasePath
	}
	return &LocalProvider{
		rootDir:        rootDir,
		scheme:         cfg.Scheme,
		host:           cfg.Domain,
		basePath:       strings.Trim(cfg.BasePath, "/"),
		signedBasePath: strings.Trim(cfg.SignedBasePath, "/"),
	}, nil
}

func (p *LocalProvider) Name() string {
	return LocalProviderName
}

// RootDir 返回本地存储根目录的绝对路径。
func (p *LocalProvider) RootDir() string {
	return p.rootDir
}

// SignedBasePath 返回签名 URL 的挂载路径，transport 层据此注册验签路由。
func (p *LocalProvider) SignedBasePath() string {
	return p.signedBasePath
}

func (p *LocalProvider) Upload(ctx context.Context, req *model.UploadRequest) (*model.ProviderResult, error) {
	_ = ctx
	reader, err := p.inputReader(req)
	if err != nil {
		return nil, err
	}
	objectKey, err := p.objectKey(req)
	if err != nil {
		return nil, err
	}
	storePath, err := p.resolve(objectKey)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(storePath), 0o750); err != nil {
		return nil, err
	}

	// 先写临时文件再原子重命名：写入中途失败不会在目标路径留下半截文件，
	// 也不会破坏同 object key 已存在的完整对象。
	tmpFile, err := os.CreateTemp(filepath.Dir(storePath), ".upload-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmpFile.Name()
	size, err := io.Copy(tmpFile, reader)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err = tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err = tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err = os.Chmod(tmpPath, 0o640); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err = os.Rename(tmpPath, storePath); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}

	return &model.ProviderResult{
		Provider:  p.Name(),
		ObjectKey: objectKey,
		StorePath: storePath,
		ETag:      req.Hash,
		Size:      size,
	}, nil
}

// Delete 删除底层文件。对象已不存在时视为删除成功，
// 否则一旦文件被外部手工清理，对应元数据将永远删不掉。
func (p *LocalProvider) Delete(ctx context.Context, objectKey string) error {
	_ = ctx
	storePath, err := p.resolve(objectKey)
	if err != nil {
		return err
	}
	if err = os.Remove(storePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (p *LocalProvider) Stat(ctx context.Context, objectKey string) (*model.ObjectStat, error) {
	_ = ctx
	storePath, err := p.resolve(objectKey)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &model.ObjectStat{Exists: false}, nil
		}
		return nil, err
	}
	return &model.ObjectStat{Exists: true, Size: info.Size(), UpdatedAt: info.ModTime()}, nil
}

// Open 实现 ObjectReader，供 private 资源的代理下载读取文件流。
func (p *LocalProvider) Open(ctx context.Context, objectKey string) (io.ReadCloser, *model.ObjectStat, error) {
	_ = ctx
	storePath, err := p.resolve(objectKey)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(storePath)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, &model.ObjectStat{Exists: true, Size: info.Size(), UpdatedAt: info.ModTime()}, nil
}

// BuildURL 生成公开直链。
//
// 不再额外拼 base_path：它已经是 object key 的一部分（见 object.PrefixKey）。
func (p *LocalProvider) BuildURL(objectKey string, opts *model.URLBuildOption) (string, error) {
	// 本地盘没有媒体处理能力。静默忽略会让调用方拿到一条原图地址
	// 却以为自己要到了缩略图——报错才能让这个差异在接入时就暴露。
	if opts != nil && opts.Process != "" {
		return "", provider.ErrProcessUnsupported
	}
	scheme, host := p.urlParts(opts)
	return object.URL(scheme, host, "", objectKey), nil
}

// provider.ErrProcessUnsupported 当前 provider 不具备媒体处理能力。

// BuildSignedURL 生成指向验签代理接口的时效地址。
func (p *LocalProvider) BuildSignedURL(ctx context.Context, objectKey string, opts *model.SignedURLBuildOption) (string, error) {
	_ = ctx
	if opts == nil {
		return "", fmt.Errorf("signed url option is nil")
	}
	if opts.SecretKey == "" {
		return "", fmt.Errorf("signed url secret is empty")
	}
	if opts.ExpireSeconds <= 0 {
		return "", fmt.Errorf("signed url expire seconds must be positive")
	}
	if opts.Process != "" {
		return "", provider.ErrProcessUnsupported
	}
	scheme := p.scheme
	host := p.host
	if opts.Scheme != "" {
		scheme = opts.Scheme
	}
	if opts.Host != "" {
		host = opts.Host
	}
	// 注意：这里刻意不使用 opts.Path / BasePath，签名地址固定挂在验签路由下。
	rawURL := object.URL(scheme, host, p.signedBasePath, objectKey)
	expireAt := time.Now().Add(time.Duration(opts.ExpireSeconds) * time.Second).Unix()
	query := provider.SignedQuery(objectKey, expireAt, opts.UserID, opts.BindUser, opts.SecretKey)
	return rawURL + "?" + query.Encode(), nil
}

func (p *LocalProvider) inputReader(req *model.UploadRequest) (io.Reader, error) {
	if req.Reader != nil {
		return req.Reader, nil
	}
	if len(req.Bytes) > 0 {
		return bytes.NewReader(req.Bytes), nil
	}
	return nil, fmt.Errorf("upload input is empty")
}

// objectKey 按 {base_path}/{namespace}/{account_id}/{biz_type}/{yyyy}/{mm}/{hash}-{random}{ext}
// 生成唯一物理对象 Key。
// 如果 namespace 或 account_id 为空，会平滑剔除对应层级，确保向下兼容。
func (p *LocalProvider) objectKey(req *model.UploadRequest) (string, error) {
	parts, err := object.IdentitySegments(req)
	if err != nil {
		return "", err
	}

	// 4. 年/月/日分桶，避免单目录下文件过多影响物理存储系统 IO 性能
	now := time.Now()
	// parts = append(parts, fmt.Sprintf("%04d/%02d/%02d", now.Year(), int(now.Month()), now.Day()))
	parts = append(parts, fmt.Sprintf("%04d/%02d", now.Year(), int(now.Month())))

	// 5. 唯一文件名与扩展名
	name, err := object.Name(req)
	if err != nil {
		return "", err
	}
	parts = append(parts, name)

	return object.PrefixKey(p.basePath, strings.Join(parts, "/")), nil
}

// resolve 把 object key 映射为本地绝对路径，并确保结果不会逃出根目录。
func (p *LocalProvider) resolve(objectKey string) (string, error) {
	// 显式拒绝 ".." 而不是静默 Clean 掉：让非法 key 直接报错，
	// 避免 "../a" 与 "a" 被折叠成同一个对象这种隐蔽的歧义。
	for _, segment := range strings.Split(objectKey, "/") {
		if segment == ".." {
			return "", fmt.Errorf("invalid object key: %q", objectKey)
		}
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(objectKey, "/"))
	if cleaned == "/" {
		return "", fmt.Errorf("invalid object key: %q", objectKey)
	}
	storePath := filepath.Join(p.rootDir, filepath.FromSlash(cleaned))
	if storePath != p.rootDir && !strings.HasPrefix(storePath, p.rootDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid object key: %q", objectKey)
	}
	return storePath, nil
}

func (p *LocalProvider) urlParts(opts *model.URLBuildOption) (string, string) {
	scheme := p.scheme
	host := p.host
	if opts != nil {
		if opts.Scheme != "" {
			scheme = opts.Scheme
		}
		if opts.Host != "" {
			host = opts.Host
		}
	}
	return scheme, host
}

var (
	_ provider.Provider          = (*LocalProvider)(nil)
	_ provider.ObjectReader      = (*LocalProvider)(nil)
	_ provider.ObjectWriter      = (*LocalProvider)(nil)
	_ provider.ObjectLister      = (*LocalProvider)(nil)
	_ provider.RangeReader       = (*LocalProvider)(nil)
	_ provider.MultipartUploader = (*LocalProvider)(nil)
)
