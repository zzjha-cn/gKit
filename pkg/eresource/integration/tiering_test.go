package integration_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

func TestInvalidEncryptionModeIsRejected(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Provider.Type = "aliyun_oss"
	cfg.Provider.Bucket = "b"
	cfg.Provider.Endpoint = "oss-cn-hangzhou.aliyuncs.com"
	cfg.Provider.AccessKey = "ak"
	cfg.Provider.SecretKey = "sk"
	cfg.Provider.ServerSideEncryption = "rot13"
	cfg.Normalize()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "server_side_encryption") {
		t.Fatalf("err = %v, want a server_side_encryption validation error", err)
	}
}

// 没有年龄下限的策略会把刚上传的热数据直接推进归档，
// 而归档取回要先解冻、按量收费。必须在校验阶段就拦住。
func TestTieringPolicyRequiresAgeFloor(t *testing.T) {
	policy := manager.TieringPolicy{Name: "cold", StorageClass: provider.StorageClassIA}
	if err := policy.Validate(); !errors.Is(err, manager.ErrInvalidTieringPolicy) {
		t.Fatalf("err = %v, want ErrInvalidTieringPolicy", err)
	}
	policy.OlderThanDays = 30
	if err := policy.Validate(); err != nil {
		t.Fatalf("a policy with an age floor should validate: %v", err)
	}
	policy.StorageClass = "Glacier"
	if err := policy.Validate(); err == nil {
		t.Fatal("an unknown storage class should be rejected")
	}
}

func TestTieringConfigIsValidated(t *testing.T) {
	cfg := baseConfig(t.TempDir())
	cfg.Maintenance = config.MaintenanceConfig{
		Enable:              true,
		TieringEveryMinutes: 60,
		TieringPolicies: []config.TieringRule{
			{Name: "cold", StorageClass: "IA"}, // 漏了 older_than_days
		},
	}
	cfg.Normalize()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "older_than_days") {
		t.Fatalf("err = %v, want an older_than_days validation error", err)
	}
}

// 本地盘不支持分层，策略执行必须明确报错而不是假装成功。
func TestApplyTieringPoliciesReportsUnsupported(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	upload(t, svc.Service, "cold.png", pngBytes())

	mgr := svc.Manager.(*manager.DefaultManager)
	_, err := mgr.ApplyTieringPolicies(context.Background(), []manager.TieringPolicy{
		{Name: "cold", OlderThanDays: 1, StorageClass: provider.StorageClassIA},
	})
	if !errors.Is(err, provider.ErrStorageClassUnsupported) {
		t.Fatalf("err = %v, want ErrStorageClassUnsupported", err)
	}
}

// 一条策略不合法不该把别的策略一起卡住。
func TestApplyTieringPoliciesIsolatesBadRules(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mgr := svc.Manager.(*manager.DefaultManager)
	_, err := mgr.ApplyTieringPolicies(context.Background(), []manager.TieringPolicy{
		{Name: "bad", StorageClass: provider.StorageClassIA}, // 缺年龄下限
		{Name: "also-bad", OlderThanDays: 1, StorageClass: "Nope"},
	})
	if err == nil {
		t.Fatal("expected both invalid policies to be reported")
	}
	if !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "also-bad") {
		t.Fatalf("err = %v, want both policy names reported", err)
	}
}

// 分层策略的全部意义在于按 biz_type 这类元数据维度选目标——
// 纯按年龄用 bucket 生命周期规则就够了，不需要代码。
func TestTieringPolicyNarrowsByBizType(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	ctx := context.Background()
	mk := func(bizType string) {
		if _, err := svc.Manager.Upload(ctx, &model.UploadRequest{
			FileName: bizType + ".png", Reader: bytes.NewReader(pngBytes()), BizType: bizType,
		}); err != nil {
			t.Fatalf("upload: %v", err)
		}
	}
	mk("material")
	mk("contract")

	// 本地 provider 不支持分层，所以这里验证的是"策略把范围收窄到了
	// 哪些资源"——用 List 走同一条 Query 路径断言。
	materials, err := svc.Manager.List(ctx, &model.Query{
		BizType:       "material",
		CreatedBefore: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(materials) != 1 || materials[0].BizType != "material" {
		t.Fatalf("policy query matched %d resources, want only the material one", len(materials))
	}
}

// 本地盘不支持分层，必须明确报错而不是假装转换成功。
func TestTransitionStorageClassUnsupportedOnLocal(t *testing.T) {
	svc := newTestService(t, nil)
	upload(t, svc, "cold.png", pngBytes())

	mgr := svc.Manager.(*manager.DefaultManager)
	_, err := mgr.TransitionStorageClass(context.Background(), &model.Query{}, provider.StorageClassIA)
	if !errors.Is(err, provider.ErrStorageClassUnsupported) {
		t.Fatalf("err = %v, want ErrStorageClassUnsupported", err)
	}
}
