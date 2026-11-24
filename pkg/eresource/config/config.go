package config

import (
	"errors"
	"fmt"
	"strings"
)

// 默认值
const (
	DefaultMaxFileSize     int64 = 32 << 20 // 32MB
	DefaultSignedURLExpire int64 = 300      // 5 分钟
	// DefaultSignedURLMaxExpire 签名地址有效期的兜底上限（秒），默认 24 小时。
	// 签发出去的地址无法撤销，所以有效期必须有天花板：
	// 没有上限时，调用方一个表单字段（expire_seconds=315360000）就能
	// 给自己签一条十年有效的永久直链，等于绕过了全部访问策略。
	DefaultSignedURLMaxExpire int64 = 86400
	DefaultRateLimitWindowSec int64 = 60 // 1 分钟
	DefaultMongoCollection          = "eresource"
	// Store / Provider 的类型标识。配置校验与告警都要按它们比对，
	// 散落的字面量迟早会写错一个而静默失效。
	StoreTypeMongo        = "mongo"
	StoreTypeJSON         = "json"
	ProviderTypeLocal     = "local"
	ProviderTypeAliyunOSS = "aliyun_oss"
	DefaultScheme         = "http"
	DefaultAccessPolicy   = "private"

	// 去重命中后的处理方式。
	DedupStrategyNewRecord   = "new_record"
	DedupStrategyReuseRecord = "reuse_record"
	DefaultDedupStrategy     = DedupStrategyNewRecord

	// 访问地址的签名模式。默认 oss_signed：由 provider 自己签发
	// （local 签向本服务的验签路由，aliyun_oss 签 OSS 预签名地址）。
	// cdn_auth_* 三型把验签下放到 CDN 边缘，源站流量基本归零，
	// 代价是频控、防盗链、下载审计对这部分流量全部失明。
	URLModeOSSSigned      = "oss_signed"
	URLModeCDNAuthA       = "cdn_auth_a"
	URLModeCDNAuthB       = "cdn_auth_b"
	URLModeCDNAuthC       = "cdn_auth_c"
	DefaultURLMode        = URLModeOSSSigned
	DefaultSignedBasePath = "resource/signed"

	// DefaultDownloadTimeoutSec 单次受控下载的超时时间（秒）。
	// 必须显著大于 HTTP 框架的单请求超时（ehttp 默认 3 秒）：
	// 回吐一个几十 MB 的对象远不止 3 秒，用请求超时来约束下载
	// 会让大文件在传到一半时被静默截断。
	DefaultDownloadTimeoutSec int64 = 300

	// DefaultUploadTimeoutSec 单次上传从落盘到登记的超时时间（秒）。
	// 与下载同理：ehttp 给每个请求挂的 3 秒超时是按「一次业务查询」定的，
	// 而把几十 MB 推给对象存储再写一条元数据远不止 3 秒。
	DefaultUploadTimeoutSec int64 = 300

	// DefaultDirectUploadExpire 前端直传凭证的有效期（秒），默认 15 分钟。
	// 这个窗口是「客户端拿到凭证到开始上传」的时间，不是上传本身的时长——
	// 表单一旦被 OSS 接收，上传过程不再受它约束。
	DefaultDirectUploadExpire int64 = 900

	// 服务端静态加密模式。
	//
	// 注意它的关键属性：**只对新写入的对象生效**。上线之后才打开，
	// 存量对象仍然是明文，只能重传一遍才能补上——所以这是一条
	// 必须在有真实数据之前就定下来的配置（Roadmap.md 阶段 1）。
	SSEModeNone   = ""
	SSEModeAES256 = "AES256"
	SSEModeKMS    = "KMS"

	// DefaultPurgeRetentionDays 开启软删除后，默认的回收保留期（天）。
	//
	// 7 天不是随便取的：误删通常要等到业务侧发现「图没了」才被报上来，
	// 这中间隔着一个周末是常态。
	DefaultPurgeRetentionDays = 7

	// DefaultMultipartExpire 分片会话默认有效期（秒）：24 小时。
	//
	// 比直传凭证（15 分钟）长得多，因为它的场景不同：直传是一次性的
	// 表单提交，分片上传要覆盖「传到一半关掉 App、第二天接着传」。
	DefaultMultipartExpire int64 = 86400

	// DefaultObjectReadSampleRate 读放大指标的默认采样分母（1%）。
	DefaultObjectReadSampleRate = 100

	// DefaultIdempotencyTTL 幂等键默认有效期：24 小时。
	//
	// 够覆盖任何合理的客户端重试窗口（包括"传到一半关掉 App、
	// 第二天接着传"），又不至于让索引长到失控。
	DefaultIdempotencyTTL int64 = 86400

	// DefaultMaxConcurrentUploads 同时处理的上传请求数上限。
	// 上传会把 MaxFileSize 以内的内容留在内存、超出部分落临时盘，
	// 两者都随并发数线性增长：没有闸门时内存上界就是 MaxFileSize × 并发数。
	DefaultMaxConcurrentUploads = 16

	DefaultProxyMaxDownloadSec         = 30       // 单次抓取超时
	DefaultProxyMaxDownloadBytes int64 = 64 << 20 // 单次抓取体积上限，64MB
)

// redacted 用于在日志中遮蔽密钥类字段。
const redacted = "[REDACTED]"

// ErrInvalidConfig 便于调用方用 errors.Is 判定配置类错误。
var ErrInvalidConfig = errors.New("eresource: invalid config")

type Config struct {
	Manager     ManagerConfig
	Store       StoreConfig
	Provider    ProviderConfig
	Access      AccessConfig
	Proxy       ResourceProxyCfg  // 资源代理与缓存抓取配置
	CDN         CDNConfig         // CDN 刷新 / 预热（可选）
	Maintenance MaintenanceConfig // 后台维护任务（可选，默认全关）
	Backup      BackupConfig      // 副本同步（可选）
}

// ResourceProxyCfg 资源抓取代理与本地中转缓存控制配置。
//
// 该能力让服务端去抓取调用方指定的 URL，是典型的 SSRF 入口，
// 因此默认关闭，必须显式 Enable 才会注册路由。
// MaintenanceConfig 后台维护任务的调度配置。
//
// 全部默认关闭。这三件事此前只是可调用的方法，需要接入方自己接定时器——
// 对大多数部署来说那是一段必然会被忘掉的代码，而忘掉的后果
// （软删对象永不清理、用量账面一路漂、孤儿对象持续计费）
// 只会在账单上才显形。
//
// 保持 opt-in 同样是刻意的：这些任务会扫全表、会删对象，
// 不该由一次依赖升级悄悄开始跑。
type MaintenanceConfig struct {
	Enable bool

	// PurgeExpiredEveryMinutes 清理过期软删资源的间隔（分钟），<=0 不跑。
	PurgeExpiredEveryMinutes int
	// PurgeLimit 单批清理条数。
	PurgeLimit int64

	// ReconcileUsageEveryMinutes 用量对账间隔（分钟），<=0 不跑。
	// 建议 1440（每天一次）。
	ReconcileUsageEveryMinutes int
	// CorrectUsage 对账时是否直接修正账面。默认只报告。
	CorrectUsage bool

	// EdgeLogEveryMinutes CDN 边缘日志回流的间隔（分钟），<=0 不跑。
	// 需要同时配 EdgeLogDomain 与 cdn.enable。
	EdgeLogEveryMinutes int
	// EdgeLogDomain 要回捞日志的 CDN 域名。
	EdgeLogDomain string
	// EdgeLogLagMinutes 回捞时向前退的分钟数，覆盖厂商的日志生成延迟。
	EdgeLogLagMinutes int
	// EdgeLogInitialLookbackMinutes 首次回捞覆盖的跨度（分钟）。
	EdgeLogInitialLookbackMinutes int

	// TieringEveryMinutes 存储分层策略的执行间隔（分钟），<=0 不跑。
	TieringEveryMinutes int
	// TieringPolicies 分层策略表：满足条件的资源转到指定存储类型。
	//
	// 纯按年龄 / 前缀的分层**应该用 bucket 生命周期规则**，不需要走这里。
	// 这张表存在的理由是表达 biz_type / 租户 / 标签这些只有元数据才知道的维度。
	TieringPolicies []TieringRule

	// ExpireIdempotencyEveryMinutes 幂等键过期清理的间隔（分钟），<=0 不跑。
	//
	// 不跑的后果是那条唯一稀疏索引随上传量无限增长。
	ExpireIdempotencyEveryMinutes int
	// ExpireIdempotencyLimit 单批清理条数。
	ExpireIdempotencyLimit int64

	// ReconcileOrphansEveryMinutes 孤儿对象对账间隔（分钟），<=0 不跑。
	ReconcileOrphansEveryMinutes int
	// OrphanSafetyWindowHours 孤儿判定的安全窗口（小时）。
	//
	// **必填**才会启用孤儿对账，没有默认值：窗口要大于最长一次上传耗时，
	// 开了直传还要大于 direct_upload_expire。给默认值等于替部署方猜，
	// 而猜错的后果是删掉用户正在传的文件。
	OrphanSafetyWindowHours int
	// OrphanBatchLimit 单批扫描的对象数。
	OrphanBatchLimit int
	// OrphanDryRun 只报告不删除。第一次在生产上跑务必先用它。
	OrphanDryRun bool
}

// TieringRule 一条存储分层规则的配置形态。
//
// 与 manager.TieringPolicy 一一对应，分开是因为配置层不该依赖 manager 包。
type TieringRule struct {
	Name          string
	Namespace     string
	AccountID     string
	BizType       string
	Tags          []string
	OlderThanDays int
	StorageClass  string // Standard / IA / Archive
	Limit         int64
}

// CDNConfig CDN 运维接口的凭证。
//
// 只影响刷新与预热，不影响 access.url_mode 的鉴权密钥——
// 那是 CDN 边缘验签用的，两把钥匙来源不同、权限也不同。
type CDNConfig struct {
	// Enable 打开后 eresource.New 会装配 cdn.AliCDNRefresher。
	Enable    bool
	Type      string // 当前只支持 aliyun_cdn，留空即它
	AccessKey string
	SecretKey string
	Endpoint  string // 留空取 https://cdn.aliyuncs.com/
}

// String 遮蔽密钥后再输出。
func (c CDNConfig) String() string {
	secret := c.SecretKey
	if secret != "" {
		secret = redacted
	}
	return fmt.Sprintf("CDNConfig{Enable:%t Type:%s AccessKey:%s SecretKey:%s Endpoint:%s}",
		c.Enable, c.Type, c.AccessKey, secret, c.Endpoint)
}

// BackupConfig 副本同步配置。
//
// 这是跨区域容灾的代码实现：按配置再装配一个 Provider 作为副本目标，
// 由调用方调度 Manager.ReplicateTo 把对象镜像过去。
//
// 与 bucket 原生的跨区域复制规则**不是替代关系**：后者是对象存储自己做的、
// 更可靠也不占业务资源，有条件优先用它。这里解决的是它覆盖不到的情况——
// 复制到另一个厂商、复制到本地盘做离线备份、或者只复制某些 biz_type。
type BackupConfig struct {
	Enable bool
	// Provider 副本目标的完整 Provider 配置（另一个 region / bucket / 甚至另一种类型）。
	Provider ProviderConfig
}

type ResourceProxyCfg struct {
	Enable         bool     // 是否启用远端抓取中转能力，默认关闭
	ProxyURL       string   // 抓取远端时可选的中转 HTTP 代理服务器
	AllowedHosts   []string // 允许进行中转抓取的域名白名单 (防恶意 SSRF 扫描内部/外部未知网络)
	CacheDir       string   // 本地中转缓存文件的存储根目录，启用时必填
	ExpireDays     int      // 缓存文件生命周期（天），<=0 表示不自动清理
	MaxDownloadSec int      // 单次远端资源下载的最大超时时间（秒），默认 30
	// MaxDownloadBytes 单次抓取允许落盘的最大字节数，默认 64MB。
	// 没有这个上限，一个超大文件链接就能把中转盘打满。
	MaxDownloadBytes int64
}

type ManagerConfig struct {
	// EnableDedup 开启内容去重（秒传）：同租户内已存在相同内容的资源时，
	// 跳过底层写入。具体复用方式由 DedupStrategy 决定。
	EnableDedup bool

	// DedupStrategy 命中去重时的处理方式，仅在 EnableDedup 打开时有意义。
	//
	//   new_record   （默认）复用底层对象，但为本次上传新建一条记录，
	//                 带上本次的文件名 / 标签 / metadata
	//   reuse_record  直接返回历史记录，本次上传的文件名 / 标签 / metadata 被丢弃
	//
	// 默认选 new_record 是因为 reuse_record 的返回值会骗人：业务拿着返回的 ID
	// 去关联「本次上传」，关到的却是历史那一条，连 creator 都是别人。
	// 代价是「记录 : 对象」从 1:1 变成 N:1，删除时必须先数引用——
	// 这部分由 Manager 内部处理，调用方无感。
	DedupStrategy string

	MaxFileSize         int64
	AllowedMimeTypes    []string
	AllowedExts         []string
	DefaultAccessPolicy string

	// SkipMimeSniffCheck 关闭「声明类型与嗅探类型一致性」校验。
	// 默认开启该校验：声明为 image/video/audio 的内容若嗅探出不同的顶级类型
	// （典型如把 HTML 伪装成 png 实现存储型 XSS）会被拒绝。
	// 仅当业务确实需要上传 SVG 等会被嗅探成 text/* 的格式时才关闭。
	SkipMimeSniffCheck bool

	// AllowPublicActiveContent 允许以 public 策略托管 HTML / SVG / XML 这类
	// 可携带脚本的内容。默认关闭。
	//
	// public 资源的直链绕过 /content 的 nosniff + CSP + attachment 防护，
	// 浏览器会直接在托管域上渲染它——这正是存储型 XSS 的标准路径。
	// 确有托管静态页 / SVG 图标的需求、且托管域与业务主域无同源关系时才打开。
	AllowPublicActiveContent bool

	// UploadTimeoutSeconds 单次上传的超时时间，单位：秒，<=0 时取默认值 300。
	//
	// 上传不能复用 HTTP 框架的单请求超时（ehttp 默认 3 秒）：推一个大文件到
	// 对象存储远超这个预算，复用它的结果是对象已经写进去、元数据还没落库，
	// 留下一个谁也查不到、也不会被清理的孤儿对象。
	UploadTimeoutSeconds int64

	// EnableDirectUpload 开放前端直传凭证接口，默认关闭。
	//
	// 打开后 /resource/upload/policy 会签发一份可直传对象存储的表单凭证，
	// 文件不再经过业务服务。凭证里锁死了 object key、Content-Type 与体积上限，
	// 但服务端看不到内容，MIME 嗅探与 public 内容拦截都退化成只认声明值——
	// 因此只在 provider 支持（当前为 aliyun_oss）且确有 C 端上传需求时才打开。
	EnableDirectUpload bool

	// DirectUploadExpire 直传凭证有效期（秒），<=0 时取默认值 900。
	DirectUploadExpire int64

	// MaxConcurrentUploads 同时处理的上传请求数上限，<=0 时取默认值 16。
	// 超出后新的上传请求直接返回 503，不排队：排队只会把压力从内存
	// 挪到连接数上，而内存才是这里真正的瓶颈。
	MaxConcurrentUploads int

	// EnableMultipartUpload 打开分片上传（init / part / complete 三段式）。
	//
	// 默认关闭。它解决的是「单次 POST 传不完」——断网之后从断点接着传，
	// 而不是 2GB 从头再来。只是文件大的话，调高 max_file_size 就够了，
	// 不需要这套复杂度。
	EnableMultipartUpload bool
	// MultipartExpire 分片会话票据的有效期（秒），<=0 时取 DefaultMultipartExpire。
	//
	// 这个值同时是"客户端最多能传多久"的上限：票据过期之后已传的分片
	// 还在底层占着空间，要靠 AbortMultipartUpload 或 bucket 生命周期规则清理。
	MultipartExpire int64

	// IdempotencyTTL 幂等键的有效期（秒），<=0 取 DefaultIdempotencyTTL。
	//
	// 过期之后键会被后台任务移除（资源记录保留）。必须有上限：
	// 幂等键是每次上传都可能带一个的高基数字段，不过期的话
	// 那条唯一稀疏索引会随上传量无限增长。
	//
	// 值的下限由业务的重试窗口决定——客户端还可能重试的时候就把键清了，
	// 那次重试会产生第二条记录，幂等等于没做。
	IdempotencyTTL int64

	// ObjectReadSampleRate 读放大指标的采样分母：100 表示 1%，1 表示全量，
	// <=0 表示不采集。默认 DefaultObjectReadSampleRate。
	//
	// 必须采样：object key 的基数等于对象总数，全量打点会在监控后端
	// 造出一个百万级 label 的时间序列。见 manager.sampleObjectKey。
	ObjectReadSampleRate int

	// EnableQuota 打开按租户的容量 / 对象数限额。
	//
	// 关闭时仍然会维护用量计数（代价是每次上传一条 $inc），
	// 只是不做拦截——先有数字再谈限额，否则第一次打开开关
	// 就会把所有存量租户一起拦死。
	EnableQuota bool
	// QuotaMaxBytes 单租户容量上限（字节），<=0 表示不限。
	QuotaMaxBytes int64
	// QuotaMaxFiles 单租户对象数上限，<=0 表示不限。
	QuotaMaxFiles int64
}

type StoreConfig struct {
	Type            string
	MongoURI        string
	MongoDB         string
	MongoCollection string
	JSONFile        string
}

type ProviderConfig struct {
	Type      string // 底层物理落盘类型（支持 local 与 aliyun_oss）
	RootDir   string // 本地存储时的底层物理根路径（仅 local 有效）
	Bucket    string // 云对象存储 Bucket 名称（如 aliyun_oss 有效）
	Endpoint  string // 云存储访问节点路径
	Region    string // 阿里云 OSS 物理区域标识（如 cn-hangzhou，用作 V4 签名核心选项）
	AccessKey string // 云存储签名凭证密钥 Key
	SecretKey string // 云存储签名凭证密钥 Secret
	// ServerSideEncryption 服务端静态加密模式，取值见 SSEMode* 常量。
	//
	// 空表示不开。只有对象存储支持，本地盘忽略该配置
	// （本地盘的加密边界在磁盘而不是在这个模块）。
	ServerSideEncryption string
	// SSEKMSKeyID 使用 KMS 模式时的密钥 ID，留空表示用 bucket 默认 CMK。
	SSEKMSKeyID string

	// AccountDomains 按租户覆盖访问域名（白标）：account_id -> 域名。
	//
	// 命中的租户用自己的域名拼直链，未命中回落 Domain。
	// 这件事之所以可行，前提是 B13 已经把域名从库里摘出来了——
	// 否则每加一个白标客户，都要把他那部分存量数据刷一遍。
	AccountDomains map[string]string

	Domain   string // 资源默认访问暴露的域名地址
	Scheme   string // 默认外链生成的访问协议，http 或 https
	BasePath string // 中间层路径前缀隔离标识

	// SignedBasePath 签名 URL 的挂载路径，必须指向由业务服务验签的代理路由，
	// 不能指向静态目录直出的公开路径。
	SignedBasePath string
}

// String 遮蔽密钥后再输出，避免 %v / %+v 打日志时泄露 OSS 凭据。
func (c ProviderConfig) String() string {
	accessKey, secretKey := c.AccessKey, c.SecretKey
	if accessKey != "" {
		accessKey = redacted
	}
	if secretKey != "" {
		secretKey = redacted
	}
	return fmt.Sprintf("ProviderConfig{Type:%s RootDir:%s Bucket:%s Endpoint:%s Region:%s AccessKey:%s SecretKey:%s Domain:%s Scheme:%s BasePath:%s SignedBasePath:%s}",
		c.Type, c.RootDir, c.Bucket, c.Endpoint, c.Region, accessKey, secretKey, c.Domain, c.Scheme, c.BasePath, c.SignedBasePath)
}

type AccessConfig struct {
	EnableHotlinkProtection bool
	RefererWhitelist        []string
	SignedURLSecret         string
	SignedURLDefaultExpire  int64

	// URLMode 受控访问地址的签名模式，取值见 URLMode* 常量。
	//
	// 切到 cdn_auth_* 之前务必先读 Roadmap.md §2.5：CDN 模式下请求
	// 根本不到业务服务，频控（AllowSignedAccess）、Referer 防盗链、
	// 下载审计与「立刻吊销」四项能力对这部分流量一并失效。
	// private 资源永远不走 CDN 鉴权，见 DefaultManager.signerFor。
	URLMode string

	// SignedURLAlignSec 把签名地址的过期时间向上对齐到该窗口（秒），0 表示不对齐。
	//
	// 不对齐时每次签出的 URL 都不同，CDN 缓存键跟着变，命中率接近 0——
	// 即使切了 cdn_auth_*，不开这个开关等于白切。
	// 代价是过期时间有最多一个窗口的抖动，且窗口内无法单独吊销某条地址。
	SignedURLAlignSec int64

	// CDNDomain CDN 鉴权地址使用的域名，留空回落 provider.domain。
	CDNDomain string
	// CDNAuthKey CDN 鉴权密钥，URLMode 为 cdn_auth_* 时必填。
	CDNAuthKey string
	// CDNAuthUID 仅 A 型鉴权使用的 uid 字段。阿里云 CDN 不校验该值，
	// 它存在的唯一意义是占住签名串里的位置，默认 "0"。
	CDNAuthUID string

	// SignedURLMaxExpire 签名地址允许的最长有效期（秒），<=0 时取
	// DefaultSignedURLMaxExpire 与 SignedURLDefaultExpire 中的较大值。
	// 调用方传入的 expire_seconds 超过该值时按该值钳制，而不是报错：
	// 上传是写操作，为一个过大的时效参数整单失败不划算。
	SignedURLMaxExpire int64

	// RequireIdentity 要求 HTTP 接入层必须拿到租户身份（namespace + account_id）。
	//
	// 租户隔离依赖网关注入 X-Resource-Namespace / X-Resource-Account-Id。
	// 漏配时上传与读取会一起落到「空租户」，功能表现完全正常、测试也全绿，
	// 没有任何信号提示隔离已经失效——这是最危险的失败模式。
	// 打开后身份头缺失直接 401，把「网关配没配对」变成上线前就能发现的事。
	RequireIdentity bool

	// TrustAdminHeader 是否信任网关注入的管理员标记头（X-Resource-Is-Admin）。
	//
	// 默认关闭：该头一旦被信任而网关又没有剥除客户端原值，
	// 任何调用方自带一个 X-Resource-Is-Admin: 1 就拿到了全租户的删除权。
	// 确认网关会无条件覆写该头之后再打开。
	TrustAdminHeader bool

	// AllowEmptyReferer 决定开启防盗链后是否放行空 Referer。
	// 浏览器在部分场景（隐私设置、跨协议跳转）确实不带 Referer，
	// 一律拒绝可能误伤正常用户，因此单独开关而不是写死。
	AllowEmptyReferer bool

	// AllowCrossCreatorDelete 允许非上传者删除资源。
	// 默认关闭：删除会连底层对象一起抹掉且不可撤销，只有上传者本人能做。
	// 后台管理、清理任务这类确实需要代删的场景才打开，
	// 打开后仍然受租户边界（namespace + account_id）约束。
	AllowCrossCreatorDelete bool

	// SoftDelete 删除时只把状态置为 deleted，保留元数据与底层对象。
	//
	// 默认关闭以保持既有行为。广告素材、合同附件这类业务建议打开：
	// 硬删除不可撤销，一次误删就是永久性的数据丢失。
	// 打开后由调用方自行调度 DefaultManager.PurgeDeleted 做延迟清理。
	SoftDelete bool

	// 频控阈值：单位时间窗口内允许的访问次数，<=0 表示该维度不限流。
	RateLimitByIP       int64
	RateLimitByUser     int64
	RateLimitByResource int64
	// RateLimitWindowSeconds 频控统计窗口长度，单位：秒。
	RateLimitWindowSeconds int64

	// AuditLogPath 内置文件审计的落盘路径，留空表示不装配。
	//
	// 配了它就会自动挂上 manager.FileAuditSink（异步缓冲 + 行式 JSON 日志）。
	// 显式用 eresource.WithAuditSink 注入的实现优先于这个配置。
	// audit_fail_closed 打开时会同时开启每次 flush 的 fsync——
	// 合规场景下"崩溃时丢了最后几条"不是一句可以接受的解释。
	AuditLogPath string

	// AuditFailClosed 审计收不下事件时是否拒绝本次访问。
	//
	// 默认关闭（丢弃并由实现自行告警）。涉及客户数据或版权素材、
	// 需要向审计方证明「每一次下载都有记录」的部署才打开——
	// 打开之后审计后端抖动会直接变成下载不可用，这是合规场景愿意付的代价，
	// 普通业务不是。
	AuditFailClosed bool

	// PrivateRedirectMinBytes private 资源超过该体积时改用 302 跳转签名地址，
	// 0 表示永远走代理读流（默认，保持现有行为）。
	//
	// 鉴权逻辑不受影响——仍然在业务服务里做完整校验，只是校验通过之后
	// 不再由业务服务回吐字节。让步的只有一点：签发之后的几十秒内
	// 那条地址可以被转发。拿它换掉的是视频类资源全部压在业务服务的出口带宽。
	PrivateRedirectMinBytes int64

	// PurgeRetentionDays 软删除资源的保留期（天）。
	//
	// 保留期是误删的唯一兜底：业务层的引用检查这个模块做不了
	// （引用关系是业务领域知识），能做的就是把「删掉了但还能救回来」
	// 的窗口留足。SoftDelete 打开而该值 <=0 时，Normalize 会补成
	// DefaultPurgeRetentionDays，不允许出现「软删了却立刻可被清理」。
	PurgeRetentionDays int

	// DownloadTimeoutSeconds 单次受控下载（/content 与签名路由）的超时时间，单位：秒。
	//
	// 下载不能复用 HTTP 框架给每个请求挂的超时（ehttp 默认 3 秒）：
	// 那个值是按「一次业务查询」定的，而回吐对象内容耗时取决于文件大小与带宽。
	// 复用它的后果是响应头已经发出、内容传到一半被掐断，客户端拿到一个
	// 截断的文件，服务端只留下一行日志——不报错、不重试的静默数据损坏。
	DownloadTimeoutSeconds int64
}

// String 遮蔽签名密钥后再输出。
func (c AccessConfig) String() string {
	secret := c.SignedURLSecret
	if secret != "" {
		secret = redacted
	}
	return fmt.Sprintf("AccessConfig{EnableHotlinkProtection:%t RefererWhitelist:%v SignedURLSecret:%s SignedURLDefaultExpire:%d SignedURLMaxExpire:%d AllowEmptyReferer:%t RequireIdentity:%t TrustAdminHeader:%t SoftDelete:%t RateLimitByIP:%d RateLimitByUser:%d RateLimitByResource:%d RateLimitWindowSeconds:%d}",
		c.EnableHotlinkProtection, c.RefererWhitelist, secret, c.SignedURLDefaultExpire, c.SignedURLMaxExpire, c.AllowEmptyReferer,
		c.RequireIdentity, c.TrustAdminHeader, c.SoftDelete,
		c.RateLimitByIP, c.RateLimitByUser, c.RateLimitByResource, c.RateLimitWindowSeconds)
}

// Normalize 填充默认值。应在 Validate 之前调用。
func (c *Config) Normalize() {
	if c.Manager.MaxFileSize <= 0 {
		c.Manager.MaxFileSize = DefaultMaxFileSize
	}
	if c.Manager.DefaultAccessPolicy == "" {
		c.Manager.DefaultAccessPolicy = DefaultAccessPolicy
	}
	if c.Manager.MaxConcurrentUploads <= 0 {
		c.Manager.MaxConcurrentUploads = DefaultMaxConcurrentUploads
	}
	if c.Manager.UploadTimeoutSeconds <= 0 {
		c.Manager.UploadTimeoutSeconds = DefaultUploadTimeoutSec
	}
	if c.Manager.DirectUploadExpire <= 0 {
		c.Manager.DirectUploadExpire = DefaultDirectUploadExpire
	}
	// 注意这里用的是 == 0 而不是 <= 0：负数是"显式关闭"，
	// 补默认值会把关闭的意图改回开启。
	if c.Manager.ObjectReadSampleRate == 0 {
		c.Manager.ObjectReadSampleRate = DefaultObjectReadSampleRate
	}
	if c.Manager.IdempotencyTTL <= 0 {
		c.Manager.IdempotencyTTL = DefaultIdempotencyTTL
	}
	if c.Manager.MultipartExpire <= 0 {
		c.Manager.MultipartExpire = DefaultMultipartExpire
	}
	if c.Manager.DedupStrategy == "" {
		c.Manager.DedupStrategy = DefaultDedupStrategy
	}
	if c.Access.URLMode == "" {
		c.Access.URLMode = DefaultURLMode
	}
	if c.Access.CDNAuthUID == "" {
		c.Access.CDNAuthUID = "0"
	}
	c.Access.CDNDomain = strings.TrimSpace(c.Access.CDNDomain)
	if c.Store.MongoCollection == "" {
		c.Store.MongoCollection = DefaultMongoCollection
	}
	if c.Provider.Scheme == "" {
		c.Provider.Scheme = DefaultScheme
	}
	if c.Provider.SignedBasePath == "" {
		c.Provider.SignedBasePath = DefaultSignedBasePath
	}
	c.Provider.BasePath = strings.Trim(c.Provider.BasePath, "/")
	c.Provider.SignedBasePath = strings.Trim(c.Provider.SignedBasePath, "/")
	if c.Access.SignedURLDefaultExpire <= 0 {
		c.Access.SignedURLDefaultExpire = DefaultSignedURLExpire
	}
	if c.Access.SignedURLMaxExpire <= 0 {
		c.Access.SignedURLMaxExpire = max(DefaultSignedURLMaxExpire, c.Access.SignedURLDefaultExpire)
	}
	if c.Access.SoftDelete && c.Access.PurgeRetentionDays <= 0 {
		c.Access.PurgeRetentionDays = DefaultPurgeRetentionDays
	}
	if c.Access.RateLimitWindowSeconds <= 0 {
		c.Access.RateLimitWindowSeconds = DefaultRateLimitWindowSec
	}
	if c.Access.DownloadTimeoutSeconds <= 0 {
		c.Access.DownloadTimeoutSeconds = DefaultDownloadTimeoutSec
	}
	if c.Proxy.MaxDownloadSec <= 0 {
		c.Proxy.MaxDownloadSec = DefaultProxyMaxDownloadSec
	}
	if c.Proxy.MaxDownloadBytes <= 0 {
		c.Proxy.MaxDownloadBytes = DefaultProxyMaxDownloadBytes
	}
	c.Proxy.CacheDir = strings.TrimSpace(c.Proxy.CacheDir)
}

// Warnings 返回配置里「能跑但很可能不是你想要的」的地方。
//
// 与 Validate 分开：这些不是错误，拦住启动过于粗暴，但静默放过
// 又恰恰是最危险的失败模式——配置能跑、测试全绿、没有任何信号，
// 直到出事才发现。eresource.New 会把它们打成 WARN 日志。
func (c Config) Warnings() []string {
	var warnings []string
	// JSONStore 是 dev-only：全量序列化重写、没有 Sync、没有并发写入保护。
	// 配着云存储却用它，意味着对象落在 OSS 上而元数据落在一个
	// 随时可能丢的本地文件里——两边的可靠性差了几个数量级。
	if c.Store.Type == StoreTypeJSON {
		if c.Provider.Type == ProviderTypeAliyunOSS {
			warnings = append(warnings,
				"store.type=json is for development only (full-file rewrite, no fsync) but provider.type=aliyun_oss looks like a production setup; use store.type=mongo")
		}
		if c.Manager.EnableQuota || c.Manager.EnableMultipartUpload || c.Backup.Enable {
			warnings = append(warnings,
				"store.type=json is for development only; quota / multipart / backup features assume a durable store")
		}
	}
	// 软删开着却没人清理，存储成本会随时间单调上涨——
	// 这条只能靠提醒，模块不内置后台任务是刻意的。
	if c.Access.SoftDelete && c.Access.PurgeRetentionDays > 0 {
		warnings = append(warnings,
			fmt.Sprintf("access.soft_delete is on with a %d-day retention; schedule DefaultManager.PurgeExpired or deleted objects will accumulate forever",
				c.Access.PurgeRetentionDays))
	}
	// 切了 CDN 鉴权却不对齐过期时间，缓存键每次都变，命中率仍然接近 0。
	if c.Access.URLMode != URLModeOSSSigned && c.Access.SignedURLAlignSec <= 0 {
		warnings = append(warnings,
			"access.url_mode is a cdn_auth_* mode but access.signed_url_align_sec is 0; every signed url differs so the CDN cache key changes on every request")
	}
	return warnings
}

// Validate 校验配置的自洽性。错误信息中不包含任何密钥内容。
func (c Config) Validate() error {
	var problems []string

	switch c.Manager.DefaultAccessPolicy {
	case "public", "protected", "private":
	default:
		problems = append(problems, fmt.Sprintf("manager.default_access_policy %q is invalid", c.Manager.DefaultAccessPolicy))
	}
	switch c.Manager.DedupStrategy {
	case DedupStrategyNewRecord, DedupStrategyReuseRecord:
	default:
		problems = append(problems, fmt.Sprintf("manager.dedup_strategy %q is invalid", c.Manager.DedupStrategy))
	}
	switch c.Access.URLMode {
	case URLModeOSSSigned:
	case URLModeCDNAuthA, URLModeCDNAuthB, URLModeCDNAuthC:
		// 没有密钥的 CDN 鉴权模式会签出一串谁都验不过的地址，
		// 而失败发生在 CDN 边缘，业务侧只能看到客户端报 403。
		if c.Access.CDNAuthKey == "" {
			problems = append(problems, "access.cdn_auth_key is required when access.url_mode is a cdn_auth_* mode")
		}
		if strings.TrimSpace(c.Access.CDNDomain) == "" && strings.TrimSpace(c.Provider.Domain) == "" {
			problems = append(problems, "access.cdn_domain or provider.domain is required when access.url_mode is a cdn_auth_* mode")
		}
	default:
		problems = append(problems, fmt.Sprintf("access.url_mode %q is invalid", c.Access.URLMode))
	}
	// 对齐窗口比有效期上限还长时，所有地址都会被对齐到超出上限的时间点，
	// 等于把上限这道兜底架空。
	if c.Access.SignedURLAlignSec > 0 && c.Access.SignedURLAlignSec > c.Access.SignedURLMaxExpire {
		problems = append(problems, fmt.Sprintf("access.signed_url_align_sec (%d) must not exceed access.signed_url_max_expire (%d)",
			c.Access.SignedURLAlignSec, c.Access.SignedURLMaxExpire))
	}
	if c.Manager.EnableQuota && c.Manager.QuotaMaxBytes <= 0 && c.Manager.QuotaMaxFiles <= 0 {
		problems = append(problems, "manager.quota_max_bytes or manager.quota_max_files is required when manager.enable_quota is on")
	}
	switch c.Provider.Scheme {
	case "http", "https":
	default:
		problems = append(problems, fmt.Sprintf("provider.scheme %q is invalid", c.Provider.Scheme))
	}
	// 非 public 默认策略必然要签发签名 URL，缺密钥应在启动时就暴露，而不是等到第一次访问。
	if c.Manager.DefaultAccessPolicy != "public" && c.Access.SignedURLSecret == "" {
		problems = append(problems, "access.signed_url_secret is required when default access policy is not public")
	}
	// Provider 必填项。这些缺失时各 Provider 的构造函数也会报错，
	// 但在这里一次性报全，比让调用方改一个跑一次要快。
	switch c.Provider.Type {
	case "local", "":
		if c.Provider.RootDir == "" {
			problems = append(problems, "provider.root_dir is required for the local provider")
		}
		// 不给默认值：public 资源的直链会持久化进元数据，漏配时若静默用 localhost，
		// 线上所有 public 资源都会存成 http://localhost/... 且只能事后刷库修复。
		if strings.TrimSpace(c.Provider.Domain) == "" {
			problems = append(problems, "provider.domain is required for the local provider")
		}
	case "aliyun_oss":
		switch c.Provider.ServerSideEncryption {
		case SSEModeNone, SSEModeAES256, SSEModeKMS:
		default:
			problems = append(problems, fmt.Sprintf("provider.server_side_encryption %q is invalid", c.Provider.ServerSideEncryption))
		}
		if c.Provider.Bucket == "" {
			problems = append(problems, "provider.bucket is required for the aliyun_oss provider")
		}
		if c.Provider.Endpoint == "" {
			problems = append(problems, "provider.endpoint is required for the aliyun_oss provider")
		}
		if c.Provider.AccessKey == "" || c.Provider.SecretKey == "" {
			problems = append(problems, "provider.access_key and provider.secret_key are required for the aliyun_oss provider")
		}
	default:
		problems = append(problems, fmt.Sprintf("provider.type %q is not supported", c.Provider.Type))
	}
	// 公开路径和验签路径重合会让签名形同虚设。
	if c.Provider.BasePath != "" && c.Provider.BasePath == c.Provider.SignedBasePath {
		problems = append(problems, "provider.base_path must differ from provider.signed_base_path")
	}
	// 上限小于默认值时，每一条按默认有效期签出的地址都会被悄悄钳短，
	// 这种自相矛盾的配置应当在启动时就报出来。
	if c.Access.SignedURLMaxExpire > 0 && c.Access.SignedURLMaxExpire < c.Access.SignedURLDefaultExpire {
		problems = append(problems, fmt.Sprintf("access.signed_url_max_expire (%d) must not be less than access.signed_url_default_expire (%d)",
			c.Access.SignedURLMaxExpire, c.Access.SignedURLDefaultExpire))
	}
	// 直传的回调登记票据用的是同一把签名密钥：没有它，任何人都能自己
	// 拼一张票据，把任意 object key 登记成自己租户下的资源。
	if c.Manager.EnableDirectUpload && c.Access.SignedURLSecret == "" {
		problems = append(problems, "access.signed_url_secret is required when manager.enable_direct_upload is on")
	}
	// 分片会话票据用的是同一把密钥：没有它，任何人都能自己拼一张票据，
	// 把分片往任意 object key 上写。
	if c.Manager.EnableMultipartUpload && c.Access.SignedURLSecret == "" {
		problems = append(problems, "access.signed_url_secret is required when manager.enable_multipart_upload is on")
	}
	if c.Access.EnableHotlinkProtection && len(c.Access.RefererWhitelist) == 0 {
		problems = append(problems, "access.referer_whitelist is required when hotlink protection is enabled")
	}
	// 缓存目录为空时 filepath.Join 会退化成相对路径，抓取到的文件会散落在进程工作目录，
	// 这既不可控也无法清理，所以启用代理时必须显式指定。
	if c.CDN.Enable {
		switch c.CDN.Type {
		case "", "aliyun_cdn":
		default:
			problems = append(problems, fmt.Sprintf("cdn.type %q is not supported", c.CDN.Type))
		}
		if c.CDN.AccessKey == "" || c.CDN.SecretKey == "" {
			problems = append(problems, "cdn.access_key and cdn.secret_key are required when cdn.enable is on")
		}
	}
	if c.Backup.Enable {
		// 副本目标指向同一个地方等于没有副本——它会把「已备份」这件事
		// 变成一句谎话，而且只有在真的要恢复时才会被发现。
		if c.Backup.Provider.Type == c.Provider.Type &&
			c.Backup.Provider.Bucket == c.Provider.Bucket &&
			c.Backup.Provider.RootDir == c.Provider.RootDir {
			problems = append(problems, "backup.provider must not point at the primary storage location")
		}
	}
	if c.Maintenance.Enable {
		for _, rule := range c.Maintenance.TieringPolicies {
			// 年龄下限缺失会让策略把刚上传的热数据直接推进归档，
			// 而归档取回要先解冻、按量收费。这是配置期就该拦住的错。
			if rule.OlderThanDays <= 0 {
				problems = append(problems,
					fmt.Sprintf("maintenance.tiering_policies[%s].older_than_days must be > 0", rule.Name))
			}
			switch rule.StorageClass {
			case "Standard", "IA", "Archive":
			default:
				problems = append(problems,
					fmt.Sprintf("maintenance.tiering_policies[%s].storage_class %q is invalid", rule.Name, rule.StorageClass))
			}
		}
		if c.Maintenance.ReconcileOrphansEveryMinutes > 0 && c.Maintenance.OrphanSafetyWindowHours <= 0 {
			problems = append(problems,
				"maintenance.orphan_safety_window_hours is required when maintenance.reconcile_orphans_every_minutes is set")
		}
		if c.Maintenance.EdgeLogEveryMinutes > 0 {
			if c.Maintenance.EdgeLogDomain == "" {
				problems = append(problems,
					"maintenance.edge_log_domain is required when maintenance.edge_log_every_minutes is set")
			}
			// 没有日志回捞实现的话，这个任务每轮都只会报一次错。
			if !c.CDN.Enable {
				problems = append(problems,
					"cdn.enable is required when maintenance.edge_log_every_minutes is set")
			}
		}
		if c.Maintenance.PurgeExpiredEveryMinutes > 0 && c.Access.PurgeRetentionDays <= 0 {
			problems = append(problems,
				"access.purge_retention_days is required when maintenance.purge_expired_every_minutes is set")
		}
	}
	if c.Proxy.Enable && c.Proxy.CacheDir == "" {
		problems = append(problems, "proxy.cache_dir is required when the resource proxy is enabled")
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidConfig, strings.Join(problems, "; "))
	}
	return nil
}
