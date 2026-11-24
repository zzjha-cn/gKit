// Package eresource 提供统一的资源管理能力：资源内容存储 + 元数据管理 + 受控 URL 暴露。
//
// 典型用法：
//
//	svc, err := eresource.New(cfg)
//	if err != nil { return err }
//	defer svc.Close()
//	result, err := svc.Manager.Upload(ctx, &model.UploadRequest{...})
//
// 需要 HTTP 接入时，把 svc 交给 transport.NewHTTPHandler 注册路由。
package eresource

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/cdn"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/aliyunoss"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/local"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
	"github.com/zzjha-cn/gKit/pkg/eresource/store/jsonstore"
	"github.com/zzjha-cn/gKit/pkg/eresource/store/mongostore"
	"go.mongodb.org/mongo-driver/mongo"
	mongooptions "go.mongodb.org/mongo-driver/mongo/options"
)

// Store 类型标识。
// 取自 config 包，避免两处字面量各自漂移——配置校验、告警与装配
// 必须认同一组取值，否则会出现"校验通过但装配不认"这种失败。
const (
	StoreTypeMongo = config.StoreTypeMongo
	StoreTypeJSON  = config.StoreTypeJSON
)

// indexTimeout 建索引的超时时间，避免启动阶段被慢 Mongo 卡死。
const indexTimeout = 10 * time.Second

// Service 是按配置装配好的一整套资源管理组件。
type Service struct {
	Config   config.Config
	Manager  manager.Manager
	Store    store.Store
	Provider provider.Provider
	// Replica 是可选的副本存储，只在 backup.enable 打开时非 nil。
	Replica provider.Provider

	// maintenance 是后台维护任务，Close 时负责停止。
	maintenance *manager.Maintenance

	// auditSink 记录由本模块自行创建的文件审计，Close 时负责冲刷并关闭。
	// 外部注入的实现由调用方自己管理生命周期。
	auditSink *manager.FileAuditSink

	// ownedMongo 记录由本模块自行创建的 Mongo 连接，Close 时负责释放。
	// 外部注入的连接由调用方自己管理生命周期。
	ownedMongo *mongo.Client
}

type options struct {
	mongoClient *mongo.Client
	store       store.Store
	provider    provider.Provider
	metrics     manager.Metrics
	audit       manager.AuditSink
	inspector   manager.ContentInspector
}

type Option func(*options)

// WithMongoClient 复用外部已有的 Mongo 连接，避免模块重复建连。
func WithMongoClient(client *mongo.Client) Option {
	return func(o *options) { o.mongoClient = client }
}

// WithStore 直接注入 Store 实现，绕过配置里的 store.type（便于测试与自定义扩展）。
func WithStore(s store.Store) Option {
	return func(o *options) { o.store = s }
}

// WithProvider 直接注入 Provider 实现，绕过配置里的 provider.type。
func WithProvider(p provider.Provider) Option {
	return func(o *options) { o.provider = p }
}

// WithMetrics 注入指标实现。不注入时所有埋点是空操作，不产生任何开销。
//
// 实现必须非阻塞且并发安全——埋点挂在上传与下载的关键路径上，
// 详见 manager.Metrics 的说明。
func WithMetrics(m manager.Metrics) Option {
	return func(o *options) { o.metrics = m }
}

// WithAuditSink 注入审计实现。不注入时审计是空操作。
//
// 实现的失败语义很重要：返回非 nil 表示「这条没收下」，
// 配合 access.audit_fail_closed 决定是丢弃还是拒绝本次访问。
func WithAuditSink(sink manager.AuditSink) Option {
	return func(o *options) { o.audit = sink }
}

// WithContentInspector 注入内容检视实现（鉴黄 / 版权 / 病毒扫描）。
// 调用在上传登记之后同步发生，详见 manager.ContentInspector。
func WithContentInspector(inspector manager.ContentInspector) Option {
	return func(o *options) { o.inspector = inspector }
}

// New 按配置装配 Store、Provider 与 Manager。
func New(cfg config.Config, opts ...Option) (*Service, error) {
	opt := &options{}
	for _, apply := range opts {
		apply(opt)
	}

	// 先补默认值再校验，配置问题在启动阶段就暴露，而不是等到第一次请求。
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// 「能跑但很可能不是你想要的」在这里打成 WARN。
	// 不做成错误是因为拦住启动过于粗暴（dev 环境就该能用 json store），
	// 但静默放过恰恰是最危险的失败模式：配置能跑、测试全绿、没有任何信号。
	for _, warning := range cfg.Warnings() {
		slog.Warn("eresource: suspicious configuration", "warning", warning)
	}

	svc := &Service{Config: cfg}

	st := opt.store
	if st == nil {
		var err error
		st, err = svc.buildStore(cfg.Store, opt.mongoClient)
		if err != nil {
			return nil, err
		}
	}

	p := opt.provider
	if p == nil {
		var err error
		p, err = NewProvider(cfg.Provider)
		if err != nil {
			svc.Close()
			return nil, err
		}
	}

	mgr, err := manager.NewDefaultManager(cfg, st, p)
	if err != nil {
		svc.Close()
		return nil, err
	}

	if opt.metrics != nil {
		mgr.SetMetrics(opt.metrics)
	}
	if opt.audit != nil {
		mgr.SetAuditSink(opt.audit)
	} else if cfg.Access.AuditLogPath != "" {
		// 配了路径就装内置的文件审计，不必再写一遍构造。
		// 显式注入的实现优先——配置只是省事的默认，不是强制。
		sink, auditErr := manager.NewFileAuditSink(manager.FileAuditOptions{
			Path:        cfg.Access.AuditLogPath,
			SyncOnFlush: cfg.Access.AuditFailClosed,
		})
		if auditErr != nil {
			svc.Close()
			return nil, fmt.Errorf("eresource: init audit log: %w", auditErr)
		}
		mgr.SetAuditSink(sink)
		svc.auditSink = sink
	}
	if opt.inspector != nil {
		mgr.SetContentInspector(opt.inspector)
	}
	// CDN 刷新：配置打开就装配，不需要调用方再写一遍构造。
	// 失败直接让 New 失败——配了却没生效的刷新能力，
	// 要到某次"收回公开"没生效时才会被发现。
	if cfg.CDN.Enable {
		refresher, cdnErr := cdn.NewAliCDNRefresher(cfg.CDN.AccessKey, cfg.CDN.SecretKey, cfg.CDN.Endpoint)
		if cdnErr != nil {
			svc.Close()
			return nil, fmt.Errorf("eresource: init cdn refresher: %w", cdnErr)
		}
		mgr.SetCDNRefresher(refresher)
		// 同一个客户端同时具备刷新 / 预热与日志回捞能力，一并挂上。
		mgr.SetCDNLogFetcher(refresher)
	}
	// 副本目标：再装配一个 Provider。它不参与任何读写主链路，
	// 只被 Manager.Replicate / VerifyReplica 使用。
	if cfg.Backup.Enable {
		replica, backupErr := NewProvider(cfg.Backup.Provider)
		if backupErr != nil {
			svc.Close()
			return nil, fmt.Errorf("eresource: init backup provider: %w", backupErr)
		}
		mgr.SetReplicaProvider(replica)
		svc.Replica = replica
	}
	// 后台维护任务：默认全关，配了才跑。
	if cfg.Maintenance.Enable {
		svc.maintenance = mgr.StartMaintenance(manager.MaintenanceOptions{
			PurgeExpiredEvery:      time.Duration(cfg.Maintenance.PurgeExpiredEveryMinutes) * time.Minute,
			PurgeLimit:             cfg.Maintenance.PurgeLimit,
			ReconcileUsageEvery:    time.Duration(cfg.Maintenance.ReconcileUsageEveryMinutes) * time.Minute,
			CorrectUsage:           cfg.Maintenance.CorrectUsage,
			EdgeLogEvery:           time.Duration(cfg.Maintenance.EdgeLogEveryMinutes) * time.Minute,
			EdgeLogDomain:          cfg.Maintenance.EdgeLogDomain,
			EdgeLogLag:             time.Duration(cfg.Maintenance.EdgeLogLagMinutes) * time.Minute,
			EdgeLogInitialLookback: time.Duration(cfg.Maintenance.EdgeLogInitialLookbackMinutes) * time.Minute,
			TieringEvery:           time.Duration(cfg.Maintenance.TieringEveryMinutes) * time.Minute,
			TieringPolicies:        tieringPolicies(cfg.Maintenance.TieringPolicies),
			ExpireIdempotencyEvery: time.Duration(cfg.Maintenance.ExpireIdempotencyEveryMinutes) * time.Minute,
			ExpireIdempotencyLimit: cfg.Maintenance.ExpireIdempotencyLimit,
			ReconcileOrphansEvery:  time.Duration(cfg.Maintenance.ReconcileOrphansEveryMinutes) * time.Minute,
			OrphanSafetyWindow:     time.Duration(cfg.Maintenance.OrphanSafetyWindowHours) * time.Hour,
			OrphanBatchLimit:       cfg.Maintenance.OrphanBatchLimit,
			OrphanDryRun:           cfg.Maintenance.OrphanDryRun,
		})
	}

	svc.Store = st
	svc.Provider = p
	svc.Manager = mgr
	return svc, nil
}

// tieringPolicies 把配置形态的分层规则翻译成 manager 的策略。
func tieringPolicies(rules []config.TieringRule) []manager.TieringPolicy {
	policies := make([]manager.TieringPolicy, 0, len(rules))
	for _, rule := range rules {
		policies = append(policies, manager.TieringPolicy{
			Name:          rule.Name,
			Namespace:     rule.Namespace,
			AccountID:     rule.AccountID,
			BizType:       rule.BizType,
			Tags:          rule.Tags,
			OlderThanDays: rule.OlderThanDays,
			StorageClass:  rule.StorageClass,
			Limit:         rule.Limit,
		})
	}
	return policies
}

// Close 释放由本模块创建的资源。外部注入的依赖不在此处关闭。
func (s *Service) Close() {
	// 维护任务先停：它会写库、删对象，不能在依赖关掉之后还在跑。
	if s.maintenance != nil {
		s.maintenance.Stop()
		s.maintenance = nil
	}
	// 审计再关：它的缓冲里可能还压着没落盘的事件，
	// 而这些事件恰恰是"关闭前最后一段时间发生了什么"。
	if s.auditSink != nil {
		_ = s.auditSink.Close()
		s.auditSink = nil
	}
	if s.ownedMongo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), indexTimeout)
		defer cancel()
		if err := s.ownedMongo.Disconnect(ctx); err != nil {
			slog.Error("eresource: disconnect mongo client", "err", err)
		}
		s.ownedMongo = nil
	}
}

func (s *Service) buildStore(cfg config.StoreConfig, client *mongo.Client) (store.Store, error) {
	switch cfg.Type {
	case StoreTypeMongo, "":
		if client == nil {
			if cfg.MongoURI == "" {
				return nil, fmt.Errorf("eresource: store.mongo_uri is empty and no mongo client provided")
			}
			ctx, cancel := context.WithTimeout(context.Background(), indexTimeout)
			defer cancel()
			clientOpts := mongooptions.Client().ApplyURI(cfg.MongoURI)
			if clientOpts.MaxPoolSize == nil || *clientOpts.MaxPoolSize == 0 {
				clientOpts.SetMaxPoolSize(10)
			}
			if clientOpts.MinPoolSize == nil || *clientOpts.MinPoolSize == 0 {
				clientOpts.SetMinPoolSize(1)
			}
			created, err := mongo.Connect(ctx, clientOpts)
			if err != nil {
				return nil, fmt.Errorf("eresource: init mongo client: %w", err)
			}
			client = created
			s.ownedMongo = created
		}
		mongoStore, err := mongostore.NewMongoStore(client, cfg.MongoDB, cfg.MongoCollection)
		if err != nil {
			s.Close()
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), indexTimeout)
		defer cancel()
		if err = mongoStore.EnsureIndexes(ctx); err != nil {
			s.Close()
			return nil, fmt.Errorf("eresource: ensure mongo indexes: %w", err)
		}
		return mongoStore, nil
	case StoreTypeJSON:
		return jsonstore.NewJSONStore(cfg.JSONFile)
	default:
		return nil, fmt.Errorf("eresource: unsupported store type %q", cfg.Type)
	}
}

// NewProvider 按配置构造 Provider 实现。
func NewProvider(cfg config.ProviderConfig) (provider.Provider, error) {
	switch cfg.Type {
	case provider.TypeLocal, "":
		return local.NewLocalProvider(cfg)
	case provider.TypeAliyunSS:
		return aliyunoss.NewAliOSSProvider(cfg)
	default:
		return nil, fmt.Errorf("eresource: unsupported provider type %q", cfg.Type)
	}
}
