package integration_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
)

// replicaService 起一个带副本目标的服务，并把两侧的根目录都交出来。
type replicaService struct {
	*eresource.Service
	root    string
	replica string
}

func newReplicaService(t *testing.T) *replicaService {
	t.Helper()
	dir := t.TempDir()
	cfg := baseConfig(dir)
	replicaRoot := filepath.Join(dir, "replica")
	cfg.Backup = config.BackupConfig{
		Enable: true,
		Provider: config.ProviderConfig{
			Type:    provider.TypeLocal,
			RootDir: replicaRoot,
			Domain:  "replica.test",
			Scheme:  "https",
		},
	}
	svc, err := eresource.New(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	return &replicaService{Service: svc, root: filepath.Join(dir, "objects"), replica: replicaRoot}
}

// 副本必须落在**完全相同的 object key** 上，否则主备对不上号，
// 恢复时每条记录都要重新映射路径。
func TestReplicateCopiesToIdenticalObjectKey(t *testing.T) {
	svc := newReplicaService(t)
	ctx := context.Background()
	body := []byte("replica me")
	result := upload(t, svc.Service, "doc.txt", body)

	mgr := svc.Manager.(*manager.DefaultManager)
	res, err := mgr.Replicate(ctx, &model.Query{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.Copied != 1 {
		t.Fatalf("copied = %d, want 1 (%+v)", res.Copied, res)
	}
	mirrored, err := os.ReadFile(filepath.Join(svc.replica, result.Resource.ObjectKey))
	if err != nil {
		t.Fatalf("read replica: %v", err)
	}
	if !bytes.Equal(mirrored, body) {
		t.Error("replica content does not match the source")
	}
}

// 幂等：跑第二遍应该全部跳过，而不是把每个对象再传一次。
func TestReplicateIsIdempotent(t *testing.T) {
	svc := newReplicaService(t)
	ctx := context.Background()
	upload(t, svc.Service, "doc.txt", []byte("replica me"))

	mgr := svc.Manager.(*manager.DefaultManager)
	if _, err := mgr.Replicate(ctx, &model.Query{}); err != nil {
		t.Fatalf("first replicate: %v", err)
	}
	second, err := mgr.Replicate(ctx, &model.Query{})
	if err != nil {
		t.Fatalf("second replicate: %v", err)
	}
	if second.Copied != 0 || second.Skipped != 1 {
		t.Fatalf("second run = %+v, want everything skipped", second)
	}
}

// 源对象没了只报告，绝不把「源没了」同步成「副本也删掉」——
// 那会把一次误删放大成两次。
func TestReplicateNeverPropagatesDeletion(t *testing.T) {
	svc := newReplicaService(t)
	ctx := context.Background()
	result := upload(t, svc.Service, "doc.txt", []byte("replica me"))

	mgr := svc.Manager.(*manager.DefaultManager)
	if _, err := mgr.Replicate(ctx, &model.Query{}); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	// 模拟源侧被误删。
	if err := os.Remove(filepath.Join(svc.root, result.Resource.ObjectKey)); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	if _, err := mgr.Replicate(ctx, &model.Query{}); err != nil {
		t.Fatalf("replicate after source loss: %v", err)
	}
	// 这是整条用例的要点：副本必须原封不动。
	if _, err := os.Stat(filepath.Join(svc.replica, result.Resource.ObjectKey)); err != nil {
		t.Errorf("replica was destroyed because the source went missing: %v", err)
	}
	// 「源没了」由反向对账报告，不是复制任务的职责——
	// 复制看到副本已存在就跳过了，根本不会去碰源对象。
	lost, err := mgr.ReconcileMissing(ctx, &model.Query{})
	if err != nil {
		t.Fatalf("reconcile missing: %v", err)
	}
	if len(lost) != 1 || lost[0] != result.Resource.ID {
		t.Fatalf("missing = %v, want the lost source reported by the reverse sweep", lost)
	}
}

// 按条件复制才是这套代码存在的理由——「只备份素材库、不备份合同附件」
// 这种维度只有元数据才知道，bucket 的复制规则表达不了。
func TestReplicateHonoursQueryFilter(t *testing.T) {
	svc := newReplicaService(t)
	ctx := context.Background()
	mk := func(bizType, name string) *model.Resource {
		result, err := svc.Manager.Upload(ctx, &model.UploadRequest{
			FileName: name,
			Reader:   bytes.NewReader([]byte(name)),
			BizType:  bizType,
		})
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		return result.Resource
	}
	material := mk("material", "a.txt")
	contract := mk("contract", "b.txt")

	mgr := svc.Manager.(*manager.DefaultManager)
	if _, err := mgr.Replicate(ctx, &model.Query{BizType: "material"}); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.replica, material.ObjectKey)); err != nil {
		t.Errorf("material was not replicated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.replica, contract.ObjectKey)); !os.IsNotExist(err) {
		t.Error("contract was replicated despite the filter")
	}
}

func TestVerifyReplicaReportsGaps(t *testing.T) {
	svc := newReplicaService(t)
	ctx := context.Background()
	result := upload(t, svc.Service, "doc.txt", []byte("replica me"))

	mgr := svc.Manager.(*manager.DefaultManager)
	missing, err := mgr.VerifyReplica(ctx, &model.Query{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(missing) != 1 || missing[0] != result.Resource.ID {
		t.Fatalf("missing = %v, want the unreplicated resource", missing)
	}
	if _, err = mgr.Replicate(ctx, &model.Query{}); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if missing, err = mgr.VerifyReplica(ctx, &model.Query{}); err != nil {
		t.Fatalf("verify after replicate: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none after replication", missing)
	}
}

func TestReplicateRequiresTarget(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mgr := svc.Manager.(*manager.DefaultManager)
	if _, err := mgr.Replicate(context.Background(), &model.Query{}); !errors.Is(err, manager.ErrNoReplicaTarget) {
		t.Fatalf("err = %v, want ErrNoReplicaTarget", err)
	}
}

// 副本目标指向主存储等于没有副本，而且只有真要恢复时才会发现。
func TestBackupTargetMustDifferFromPrimary(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(dir)
	cfg.Backup = config.BackupConfig{Enable: true, Provider: cfg.Provider}
	cfg.Normalize()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "backup.provider") {
		t.Fatalf("err = %v, want a backup.provider validation error", err)
	}
}
