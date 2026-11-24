package aliyunoss

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/internal/object"
)

const AliOSSProviderName = provider.TypeAliyunSS

// AliOSSProvider 阿里云 OSS V4 存储驱动（基于官方 OSS SDK 重新实现，具备极强的操作扩展性）
// encryptionOptions 返回静态加密相关的上传选项。
//
// 未配置时返回空切片，请求与开启前完全一致——这一点很重要：
// 加密是 bucket 级也可以配的，模块这边不该在没要求的时候擅自塞一个头进去，
// 那会覆盖掉 bucket 上已有的默认加密策略。
func (p *AliOSSProvider) encryptionOptions() []oss.Option {
	switch p.sseMode {
	case config.SSEModeAES256:
		return []oss.Option{oss.ServerSideEncryption("AES256")}
	case config.SSEModeKMS:
		opts := []oss.Option{oss.ServerSideEncryption("KMS")}
		if p.sseKeyID != "" {
			opts = append(opts, oss.ServerSideEncryptionKeyID(p.sseKeyID))
		}
		return opts
	default:
		return nil
	}
}

type AliOSSProvider struct {
	client         *oss.Client // 官方 OSS 客户端
	bucketName     string      // Bucket 桶名称
	endpoint       string      // 节点主机地址 (如 oss-cn-hangzhou.aliyuncs.com)
	region         string      // 地域标识 (如 cn-hangzhou)，V4 算法强制必须
	accessKey      string      // 访问凭证
	secretKey      string      // 签名凭证
	domain         string      // 独立访问 CDN/CNAME 域名
	useCname       bool        // 是否以 CNAME 模式签名（domain 非空时启用）
	scheme         string      // 默认外链协议
	basePath       string      // 业务中间层隔离前缀
	signedBasePath string      // 签名挂载路径
	sseMode        string      // 服务端静态加密模式，空表示不开
	sseKeyID       string      // KMS 模式下的密钥 ID
}

// NewAliOSSProvider 基于配置与官方 Go SDK 构造具备官方 V4 签名支持的存储驱动
func NewAliOSSProvider(cfg config.ProviderConfig) (*AliOSSProvider, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("aliyun oss bucket is required")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("aliyun oss endpoint is required")
	}
	if cfg.Region == "" {
		cfg.Region = extractRegion(cfg.Endpoint)
		if cfg.Region == "" {
			return nil, fmt.Errorf("aliyun oss region is required for V4 signing, couldn't extract from endpoint: %s", cfg.Endpoint)
		}
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("aliyun oss credentials (accessKey/secretKey) are required")
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "https"
	}
	if cfg.SignedBasePath == "" {
		cfg.SignedBasePath = config.DefaultSignedBasePath
	}

	// 格式化 endpoint / domain，确保去除 http:// 或 https:// 前缀
	epClean := trimScheme(cfg.Endpoint)
	domain := trimScheme(cfg.Domain)

	// 1. 初始化官方 OSS SDK 客户端，并显式指定启用阿里云最新的 Signature V4 签名算法支持。
	// 根据阿里云官方规范：启用 V4 时必须通过 oss.AuthVersion(oss.AuthV4) 与 oss.Region(region) 组合传参。
	//
	// 2. 配了自定义域名（CDN/CNAME）时，必须把域名当作 endpoint 交给 SDK 并打开 oss.UseCname，
	// 让签名从一开始就按该域名计算。V4 签名把 Host 算进 canonical request，
	// 事后改写 URL 的 Host 会让签名直接失效（表现为访问时 403 SignatureDoesNotMatch）。
	signEndpoint := epClean
	useCname := domain != ""
	if useCname {
		signEndpoint = domain
	}
	options := []oss.ClientOption{
		oss.AuthVersion(oss.AuthV4),
		oss.Region(cfg.Region),
	}
	if useCname {
		options = append(options, oss.UseCname(true))
	}
	client, err := oss.New(signEndpoint, cfg.AccessKey, cfg.SecretKey, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to init aliyun oss client: %w", err)
	}

	return &AliOSSProvider{
		client:         client,
		bucketName:     cfg.Bucket,
		endpoint:       epClean,
		region:         cfg.Region,
		accessKey:      cfg.AccessKey,
		secretKey:      cfg.SecretKey,
		domain:         domain,
		useCname:       useCname,
		scheme:         cfg.Scheme,
		basePath:       strings.Trim(cfg.BasePath, "/"),
		signedBasePath: strings.Trim(cfg.SignedBasePath, "/"),
		sseMode:        cfg.ServerSideEncryption,
		sseKeyID:       cfg.SSEKMSKeyID,
	}, nil
}

// trimScheme 去掉配置里可能带上的协议前缀与尾部斜杠，只保留主机部分。
func trimScheme(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	return strings.TrimRight(raw, "/")
}

func (p *AliOSSProvider) Name() string {
	return AliOSSProviderName
}

// objectACL 把访问策略映射成 OSS 对象 ACL。
//
// 不显式设置 ACL 时对象继承 bucket ACL，而这两种 bucket 配法都是错的：
//   - bucket 为 public-read：所有 private / protected 对象在 OSS 侧公开可读，
//     本模块的签名体系被整体绕过——URL 一旦泄露就再没有第二道防线；
//   - bucket 为 private：public 资源的固定直链必然 403，功能直接不可用。
//
// 因此 ACL 必须逐对象跟随访问策略，bucket 本身应保持 private。
//
// 认不出的策略一律落到 private：失败要失败在「访问不到」这一侧，
// 绝不能让一个没写对的策略字段把对象放成公开。
func objectACL(accessPolicy string) oss.ACLType {
	if accessPolicy == model.AccessPolicyPublic {
		return oss.ACLPublicRead
	}
	return oss.ACLPrivate
}

// Upload 基于官方 SDK 与 V4 规范上传资源文件
func (p *AliOSSProvider) Upload(ctx context.Context, req *model.UploadRequest) (*model.ProviderResult, error) {
	reader, err := p.inputReader(req)
	if err != nil {
		return nil, err
	}

	objectKey, err := p.objectKey(req)
	if err != nil {
		return nil, err
	}

	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return nil, fmt.Errorf("failed to get bucket instance: %w", err)
	}

	contentType := req.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// 利用官方 SDK 的 Option 控制请求属性。
	// oss.WithContext 必须带上，否则上传既不可取消也不受上游超时约束。
	// oss.ObjectACL 同样不能省：见 objectACL 的说明。
	options := []oss.Option{
		oss.ContentType(contentType),
		oss.ObjectACL(objectACL(req.AccessPolicy)),
		oss.WithContext(ctx),
	}
	options = append(options, p.encryptionOptions()...)

	// 统一流式或全量内存字节上传
	var length int64
	if req.Bytes != nil {
		length = int64(len(req.Bytes))
		err = bucket.PutObject(objectKey, bytes.NewReader(req.Bytes), options...)
	} else {
		// 如果是流，我们需要临时记录大小，或通过 ReadAll 取大小
		bodyBytes, readErr := io.ReadAll(reader)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read upload body stream: %w", readErr)
		}
		length = int64(len(bodyBytes))
		err = bucket.PutObject(objectKey, bytes.NewReader(bodyBytes), options...)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to put object to aliyun oss: %w", err)
	}

	// 官方 SDK 自动计算或拉取 Meta。若需要精确 ETag，可以用 Head 拿，或直接拼凑
	// 这里通过 storePath 作为完整定位，满足 model 约定
	storePath := fmt.Sprintf("%s.%s/%s", p.bucketName, p.endpoint, objectKey)

	return &model.ProviderResult{
		Provider:  p.Name(),
		Bucket:    p.bucketName,
		ObjectKey: objectKey,
		StorePath: storePath,
		Size:      length,
	}, nil
}

// Delete 基于官方 SDK 物理删除文件
func (p *AliOSSProvider) Delete(ctx context.Context, objectKey string) error {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return fmt.Errorf("failed to get bucket: %w", err)
	}

	err = bucket.DeleteObject(objectKey, oss.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to delete object from aliyun oss: %w", err)
	}
	return nil
}

// Stat 基于官方 SDK 核验文件状态（拉取 Size、最后更新时间）
func (p *AliOSSProvider) Stat(ctx context.Context, objectKey string) (*model.ObjectStat, error) {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return nil, fmt.Errorf("failed to get bucket: %w", err)
	}

	meta, err := bucket.GetObjectDetailedMeta(objectKey, oss.WithContext(ctx))
	if err != nil {
		// 统一将 404 等不存在的底层错误适配为 model.ObjectStat{Exists: false}
		if isNotFoundErr(err) {
			return &model.ObjectStat{Exists: false}, nil
		}
		return nil, fmt.Errorf("failed to get object meta: %w", err)
	}
	return objectStatFromHeader(meta), nil
}

// Open 读取对象内容，实现 ObjectReader。
//
// 有了它，private 资源就能走服务端代理下载（由业务服务读流回吐），
// 而不必把签名地址暴露给客户端。
// 调用方负责关闭返回的 ReadCloser。
func (p *AliOSSProvider) Open(ctx context.Context, objectKey string) (io.ReadCloser, *model.ObjectStat, error) {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get bucket: %w", err)
	}

	// 用 DoGetObject 而不是 GetObject：前者连同响应头一起返回，
	// 一次往返就能同时拿到内容流与 Size / Content-Type，不必再单独 HEAD 一次。
	result, err := bucket.DoGetObject(&oss.GetObjectRequest{ObjectKey: objectKey}, []oss.Option{oss.WithContext(ctx)})
	if err != nil {
		if isNotFoundErr(err) {
			// 包装成 fs.ErrNotExist，与 LocalProvider 的行为对齐，
			// 上层据此区分「对象不存在」与「存储不可用」。
			return nil, nil, fmt.Errorf("%w: object %q", fs.ErrNotExist, objectKey)
		}
		return nil, nil, fmt.Errorf("failed to get object from aliyun oss: %w", err)
	}
	return result.Response.Body, objectStatFromHeader(result.Response.Headers), nil
}

// objectStatFromHeader 从响应头解析对象状态。Stat 与 Open 共用，避免两处解析逻辑出现分歧。
func objectStatFromHeader(header http.Header) *model.ObjectStat {
	stat := &model.ObjectStat{
		Exists:      true,
		ContentType: header.Get("Content-Type"),
	}
	if size, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64); err == nil {
		stat.Size = size
	}
	// OSS 返回的是 HTTP 日期格式，用 http.ParseTime 覆盖 RFC1123 等几种合法写法。
	if lastModified, err := http.ParseTime(header.Get("Last-Modified")); err == nil {
		stat.UpdatedAt = lastModified
	}
	return stat
}

// isNotFoundErr 判断 SDK 错误是否表示对象不存在。
//
// 不能用 strings.Contains 匹配错误文本：那依赖 SDK 的错误格式，
// 版本一变就会静默失效，而且 Message 里恰好出现 NoSuchKey 也会误判。
func isNotFoundErr(err error) bool {
	var serviceErr oss.ServiceError
	if errors.As(err, &serviceErr) {
		return serviceErr.StatusCode == http.StatusNotFound ||
			serviceErr.Code == "NoSuchKey" || serviceErr.Code == "NoSuchBucket"
	}
	// HEAD 之类没有响应体的请求，SDK 会退化成只带状态码的错误。
	var statusErr oss.UnexpectedStatusCodeError
	if errors.As(err, &statusErr) {
		return statusErr.Got() == http.StatusNotFound
	}
	return false
}

// BuildURL 生成无需签名的普通访问外链，优先采用 CDN/CNAME 自定义域名
func (p *AliOSSProvider) BuildURL(objectKey string, opts *model.URLBuildOption) (string, error) {
	scheme := p.scheme
	host := p.domain
	if host == "" {
		host = fmt.Sprintf("%s.%s", p.bucketName, p.endpoint)
	}

	if opts != nil {
		if opts.Scheme != "" {
			scheme = opts.Scheme
		}
		if opts.Host != "" {
			host = opts.Host
		}
	}

	// 与 LocalProvider 共用逐段 PathEscape 的拼接逻辑。base_path 在 OSS 下已经是
	// object key 的一部分，因此这里不再额外拼路径前缀。
	raw := object.URL(scheme, host, "", objectKey)
	if opts != nil && opts.Process != "" {
		raw += "?x-oss-process=" + url.QueryEscape(opts.Process)
	}
	return raw, nil
}

// BuildSignedURL 基于官方 Go SDK V4 签名机制，生成临时预签名 URL 地址。
//
// 注意：V4 签名把 Host 算进 canonical request，签名结果与域名绑定。
// 因此这里不接受 opts.Host / opts.Scheme 的覆盖——签完之后改写 URL 的 Host
// 会让签名立刻失效（访问时 403 SignatureDoesNotMatch）。
// 自定义域名（CDN/CNAME）通过 config.Provider.Domain 配置，
// 在构造客户端时就以 oss.UseCname 的方式生效，见 NewAliOSSProvider。
func (p *AliOSSProvider) BuildSignedURL(ctx context.Context, objectKey string, opts *model.SignedURLBuildOption) (string, error) {
	bucket, err := p.client.Bucket(p.bucketName)
	if err != nil {
		return "", fmt.Errorf("failed to get bucket: %w", err)
	}

	// opts 可能为 nil（Provider 是导出接口，调用方不受控），过期时间也必须为正数，
	// 否则 SDK 会签出一个立刻过期、无法访问的链接。
	expireSeconds := int64(0)
	if opts != nil {
		expireSeconds = opts.ExpireSeconds
	}
	if expireSeconds <= 0 {
		expireSeconds = config.DefaultSignedURLExpire
	}

	signOptions := []oss.Option{oss.WithContext(ctx)}
	// 处理参数必须参与签名：附加在签好的地址后面会被 OSS 算进
	// canonical query，签名随即对不上，表现是 403 而不是「没处理」。
	if opts != nil && opts.Process != "" {
		signOptions = append(signOptions, oss.Process(opts.Process))
	}
	signedURLStr, err := bucket.SignURL(objectKey, oss.HTTPGet, expireSeconds, signOptions...)
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned url by sdk: %w", err)
	}
	return signedURLStr, nil
}

func (p *AliOSSProvider) inputReader(req *model.UploadRequest) (io.Reader, error) {
	if req.Reader != nil {
		return req.Reader, nil
	}
	if len(req.Bytes) > 0 {
		return bytes.NewReader(req.Bytes), nil
	}
	return nil, fmt.Errorf("upload payload is empty")
}

func (p *AliOSSProvider) objectKey(req *model.UploadRequest) (string, error) {
	parts, err := object.IdentitySegments(req)
	if err != nil {
		return "", err
	}

	// 4. 时间路由分区
	now := time.Now()
	parts = append(parts, fmt.Sprintf("%04d/%02d/%02d", now.Year(), int(now.Month()), now.Day()))

	// 5. 实体文件
	name, err := object.Name(req)
	if err != nil {
		return "", err
	}
	parts = append(parts, name)

	return object.PrefixKey(p.basePath, strings.Join(parts, "/")), nil
}

// regionPattern 地域标识的形态：两位地理前缀 + '-' + 其余部分，
// 如 cn-hangzhou、us-west-1、ap-southeast-2、rg-china-mainland。
var regionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z0-9-]+$`)

// extractRegion 尝试从 endpoint 推断 V4 签名所需的地域标识。
//
// 只接受形态上确实像地域的结果，推断不出就返回空串让调用方显式报错。
// 早先的实现把 "oss-" 后面的内容原样当作 region，于是
// oss-accelerate.aliyuncs.com 会推断出 "accelerate"、
// oss-cn-hangzhou-internal.aliyuncs.com 会推断出 "cn-hangzhou-internal"，
// 两者都不是合法地域，但要等到第一次请求签名失败才会暴露。
func extractRegion(endpoint string) string {
	host := trimScheme(endpoint)
	// 去掉端口，只看主机名
	if idx := strings.IndexByte(host, ':'); idx >= 0 {
		host = host[:idx]
	}
	for _, part := range strings.Split(strings.ToLower(host), ".") {
		candidate, ok := strings.CutPrefix(part, "oss-")
		if !ok {
			continue
		}
		// 内网 / 传输加速等后缀不属于地域标识本身
		for _, suffix := range []string{"-internal", "-extranet", "-vpc"} {
			candidate = strings.TrimSuffix(candidate, suffix)
		}
		if regionPattern.MatchString(candidate) {
			return candidate
		}
	}
	return ""
}

var (
	_ provider.Provider           = (*AliOSSProvider)(nil)
	_ provider.ObjectReader       = (*AliOSSProvider)(nil)
	_ provider.ObjectWriter       = (*AliOSSProvider)(nil)
	_ provider.ObjectLister       = (*AliOSSProvider)(nil)
	_ provider.RangeReader        = (*AliOSSProvider)(nil)
	_ provider.MultipartUploader  = (*AliOSSProvider)(nil)
	_ provider.DirectUploader     = (*AliOSSProvider)(nil)
	_ provider.ACLSetter          = (*AliOSSProvider)(nil)
	_ provider.StorageClassSetter = (*AliOSSProvider)(nil)
)
