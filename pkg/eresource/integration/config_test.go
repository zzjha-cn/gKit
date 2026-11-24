package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/local"
	"github.com/zzjha-cn/gKit/pkg/eresource/store/jsonstore"
)

// 修复点 15：配置校验与密钥脱敏。
func TestConfigValidationAndRedaction(t *testing.T) {
	// 每个用例只故意坏一处，其余字段都补齐，确保断言到的就是它声称的那条规则
	localProvider := func() config.ProviderConfig {
		// 带上 Domain：否则每个基于它的用例都会因「缺 domain」被拒，
		// 掩盖掉它真正要验证的那条规则。
		return config.ProviderConfig{Type: "local", RootDir: "/tmp/eresource-cfg-test", Domain: "cdn.test"}
	}
	cases := map[string]config.Config{
		"bad access policy": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "wide-open"},
			Provider: localProvider(),
		},
		"bad scheme": func() config.Config {
			p := localProvider()
			p.Scheme = "ftp"
			return config.Config{Manager: config.ManagerConfig{DefaultAccessPolicy: "public"}, Provider: p}
		}(),
		"missing secret": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "protected"},
			Provider: localProvider(),
		},
		"hotlink without whitelist": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "public"},
			Provider: localProvider(),
			Access:   config.AccessConfig{EnableHotlinkProtection: true},
		},
		"signed path collides with public path": func() config.Config {
			p := localProvider()
			p.BasePath, p.SignedBasePath = "files", "files"
			return config.Config{Manager: config.ManagerConfig{DefaultAccessPolicy: "public"}, Provider: p}
		}(),
		// 修复点 16：Provider 必填项也要在启动阶段查出来
		"local without root_dir": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "public"},
			Provider: config.ProviderConfig{Type: "local", Domain: "cdn.test"},
		},
		// public 直链会持久化进元数据，漏配 domain 不能静默落到 localhost
		"local without domain": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "public"},
			Provider: config.ProviderConfig{Type: "local", RootDir: "/tmp/eresource-cfg-test"},
		},
		"local with blank domain": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "public"},
			Provider: config.ProviderConfig{Type: "local", RootDir: "/tmp/eresource-cfg-test", Domain: "   "},
		},
		"aliyun without bucket": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "public"},
			Provider: config.ProviderConfig{Type: "aliyun_oss", Endpoint: "oss-cn-hangzhou.aliyuncs.com", AccessKey: "ak", SecretKey: "sk"},
		},
		"aliyun without endpoint": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "public"},
			Provider: config.ProviderConfig{Type: "aliyun_oss", Bucket: "b", AccessKey: "ak", SecretKey: "sk"},
		},
		"aliyun without credentials": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "public"},
			Provider: config.ProviderConfig{Type: "aliyun_oss", Bucket: "b", Endpoint: "oss-cn-hangzhou.aliyuncs.com"},
		},
		"unknown provider type": {
			Manager:  config.ManagerConfig{DefaultAccessPolicy: "public"},
			Provider: config.ProviderConfig{Type: "s3"},
		},
	}
	for name, cfg := range cases {
		cfg.Normalize()
		if err := cfg.Validate(); !errors.Is(err, config.ErrInvalidConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidConfig", name, err)
		}
	}

	valid := config.Config{
		Manager:  config.ManagerConfig{DefaultAccessPolicy: "protected"},
		Access:   config.AccessConfig{SignedURLSecret: "s3cr3t"},
		Provider: config.ProviderConfig{Type: "local", RootDir: "/tmp/eresource-cfg-test", Domain: "cdn.test", AccessKey: "AKID-real", SecretKey: "sk-real"},
	}
	valid.Normalize()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if valid.Manager.MaxFileSize != config.DefaultMaxFileSize || valid.Provider.Scheme != config.DefaultScheme {
		t.Errorf("defaults not applied: %+v", valid.Manager)
	}

	// 密钥不得出现在日志输出里
	dump := fmt.Sprintf("%v|%+v", valid.Provider, valid.Access)
	for _, secret := range []string{"AKID-real", "sk-real", "s3cr3t"} {
		if strings.Contains(dump, secret) {
			t.Errorf("secret %q leaked into %q", secret, dump)
		}
	}
}

// 缺少签名密钥时宁可报错，也不能退回到硬编码默认盐。
// 默认策略非 public 时配置校验会直接拦下；默认 public 但单个资源声明受控时，
// 在签发环节报错。
func TestSignedURLRequiresConfiguredSecret(t *testing.T) {
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyPublic
		cfg.Access.SignedURLSecret = ""
	})
	_, err := svc.Manager.Upload(context.Background(), &model.UploadRequest{
		FileName: "doc.pdf", Reader: strings.NewReader("pdf"),
		AccessPolicy: model.AccessPolicyProtected, Creator: "alice",
	})
	if !errors.Is(err, manager.ErrSignedURLSecretMissing) {
		t.Fatalf("err = %v, want ErrSignedURLSecretMissing", err)
	}
}

// 工厂装配：配置里的类型必须真的决定实现。
func TestFactoryWiring(t *testing.T) {
	svc := newTestService(t, nil)
	if _, ok := svc.Store.(*jsonstore.JSONStore); !ok {
		t.Errorf("store type = %T, want *jsonstore.JSONStore", svc.Store)
	}
	if _, ok := svc.Provider.(*local.LocalProvider); !ok {
		t.Errorf("provider type = %T, want *local.LocalProvider", svc.Provider)
	}
	if _, err := eresource.New(config.Config{Store: config.StoreConfig{Type: "redis"}}); err == nil {
		t.Error("expected unsupported store type to fail")
	}
	if _, err := eresource.New(config.Config{
		Store:    config.StoreConfig{Type: eresource.StoreTypeJSON, JSONFile: filepath.Join(t.TempDir(), "m.json")},
		Provider: config.ProviderConfig{Type: provider.TypeAliyunSS},
	}); err == nil {
		t.Error("expected not-implemented provider to fail loudly")
	}
}

// 直接调用构造函数（绕过 eresource.New 的配置校验）同样不能静默落到 localhost。
func TestNewLocalProviderRejectsEmptyDomain(t *testing.T) {
	for _, domain := range []string{"", "  "} {
		_, err := local.NewLocalProvider(config.ProviderConfig{
			RootDir: t.TempDir(), Domain: domain,
		})
		if err == nil {
			t.Errorf("domain = %q 时构造成功，期望报错——public 直链会被存成 localhost", domain)
		}
	}
}

func TestWarnsAboutJSONStoreInProductionShape(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Provider.Type = config.ProviderTypeAliyunOSS
	cfg.Provider.Bucket = "b"
	cfg.Provider.Endpoint = "oss-cn-hangzhou.aliyuncs.com"
	cfg.Provider.AccessKey = "ak"
	cfg.Provider.SecretKey = "sk"
	cfg.Normalize()

	warnings := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(warnings, "store.type=json") {
		t.Fatalf("warnings = %q, want a json-store warning", warnings)
	}
	// 告警不是错误：dev 环境必须还能跑起来。
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the warning must not block startup: %v", err)
	}
}

func TestNoWarningForADevShapedConfig(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Normalize()
	for _, warning := range cfg.Warnings() {
		if strings.Contains(warning, "store.type=json") {
			t.Fatalf("a local dev config should not warn about the json store: %q", warning)
		}
	}
}
