package integration_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource"
	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider"
	"github.com/zzjha-cn/gKit/pkg/eresource/provider/local"
)

func newMultipartService(t *testing.T, tune func(*config.Config)) *serviceWithRoot {
	t.Helper()
	return newServiceWithRoot(t, func(c *config.Config) {
		c.Manager.EnableMultipartUpload = true
		c.Manager.MaxFileSize = 64 << 20
		if tune != nil {
			tune(c)
		}
	})
}

func initSession(t *testing.T, svc *eresource.Service, name string, totalSize int64) *model.MultipartSession {
	t.Helper()
	mp := svc.Manager.(manager.MultipartUploader)
	session, err := mp.InitMultipartUpload(context.Background(), &model.MultipartInitRequest{
		FileName:  name,
		BizType:   "video",
		TotalSize: totalSize,
	})
	if err != nil {
		t.Fatalf("init multipart: %v", err)
	}
	return session
}

// 三段式跑通：分片乱序上传，合并之后内容必须与原文逐字节一致。
func TestMultipartUploadRoundTrip(t *testing.T) {
	svc := newMultipartService(t, nil)
	ctx := context.Background()
	mp := svc.Manager.(manager.MultipartUploader)

	// 两片：首片必须 >= 5MB（对象存储的硬性要求），末片不限。
	first := bytes.Repeat([]byte("a"), int(model.MinPartSize))
	second := bytes.Repeat([]byte("b"), 1024)
	whole := append(append([]byte(nil), first...), second...)

	session := initSession(t, svc.Service, "movie.bin", int64(len(whole)))

	// 刻意倒序上传，验证服务端按 part_number 排序而不是按到达顺序拼接。
	p2, err := mp.UploadPart(ctx, session.Ticket, 2, bytes.NewReader(second), int64(len(second)), nil)
	if err != nil {
		t.Fatalf("upload part 2: %v", err)
	}
	p1, err := mp.UploadPart(ctx, session.Ticket, 1, bytes.NewReader(first), int64(len(first)), nil)
	if err != nil {
		t.Fatalf("upload part 1: %v", err)
	}

	result, err := mp.CompleteMultipartUpload(ctx, &model.MultipartCompleteRequest{
		Ticket: session.Ticket,
		Parts:  []model.MultipartPart{*p2, *p1},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if result.Resource.Size != int64(len(whole)) {
		t.Fatalf("size = %d, want %d", result.Resource.Size, len(whole))
	}

	stored, err := os.ReadFile(filepath.Join(svc.root, result.Resource.ObjectKey))
	if err != nil {
		t.Fatalf("read merged object: %v", err)
	}
	if !bytes.Equal(stored, whole) {
		t.Fatal("merged object does not match the original content")
	}
	// 分片暂存目录必须清干净，否则每传一个大文件就留一份完整副本。
	entries, _ := os.ReadDir(filepath.Join(svc.root, ".multipart"))
	if len(entries) != 0 {
		t.Errorf("staging dir still holds %d sessions", len(entries))
	}
}

// 合并后按**实际**大小再卡一次体积：init 时客户端声明的总大小不可信。
func TestMultipartRejectsOversizedMergeAndCleansUp(t *testing.T) {
	svc := newMultipartService(t, func(c *config.Config) {
		c.Manager.MaxFileSize = model.MinPartSize + 512
	})
	ctx := context.Background()
	mp := svc.Manager.(manager.MultipartUploader)

	// 声明一个小得多的总大小，实际传超限。
	session := initSession(t, svc.Service, "huge.bin", 1024)
	first := bytes.Repeat([]byte("a"), int(model.MinPartSize))
	second := bytes.Repeat([]byte("b"), 4096)

	p1, err := mp.UploadPart(ctx, session.Ticket, 1, bytes.NewReader(first), int64(len(first)), nil)
	if err != nil {
		t.Fatalf("upload part 1: %v", err)
	}
	p2, err := mp.UploadPart(ctx, session.Ticket, 2, bytes.NewReader(second), int64(len(second)), nil)
	if err != nil {
		t.Fatalf("upload part 2: %v", err)
	}
	_, err = mp.CompleteMultipartUpload(ctx, &model.MultipartCompleteRequest{
		Ticket: session.Ticket,
		Parts:  []model.MultipartPart{*p1, *p2},
	})
	if !errors.Is(err, manager.ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
	// 超限的对象不能留在存储里：既没拦住上传又多一个孤儿是最差的结果。
	if _, statErr := os.Stat(filepath.Join(svc.root, session.ObjectKey)); !os.IsNotExist(statErr) {
		t.Error("an oversized merged object was left behind")
	}
	total, err := svc.Manager.Count(ctx, &model.Query{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 0 {
		t.Errorf("a rejected multipart upload left %d records", total)
	}
}

// 非末片小于 5MB 必须拒绝——本地也强制，否则开发时能传的分片线上会被 OSS 拒掉。
func TestMultipartRejectsUndersizedNonFinalPart(t *testing.T) {
	svc := newMultipartService(t, nil)
	ctx := context.Background()
	mp := svc.Manager.(manager.MultipartUploader)
	session := initSession(t, svc.Service, "small.bin", 2048)

	parts := []model.MultipartPart{
		{PartNumber: 1, Size: 1024},
		{PartNumber: 2, Size: 1024},
	}
	_, err := mp.CompleteMultipartUpload(ctx, &model.MultipartCompleteRequest{
		Ticket: session.Ticket,
		Parts:  parts,
	})
	if !errors.Is(err, manager.ErrPartTooSmall) {
		t.Fatalf("err = %v, want ErrPartTooSmall", err)
	}
}

// 票据是不记名字符串，泄露出去就是一次可用的写入许可——必须比对身份。
func TestMultipartTicketIsBoundToCaller(t *testing.T) {
	svc := newMultipartService(t, nil)
	ctx := context.Background()
	mp := svc.Manager.(manager.MultipartUploader)

	session, err := mp.InitMultipartUpload(ctx, &model.MultipartInitRequest{
		FileName:  "movie.bin",
		BizType:   "video",
		Namespace: "crm",
		AccountID: "acct-a",
		Creator:   "u-1",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	intruder := &model.AccessRequest{Namespace: "crm", AccountID: "acct-b", UserID: "u-9"}
	_, err = mp.UploadPart(ctx, session.Ticket, 1, bytes.NewReader([]byte("x")), 1, intruder)
	if !errors.Is(err, manager.ErrDirectUploadIdentityMismatch) {
		t.Fatalf("err = %v, want an identity mismatch", err)
	}
}

// 被改写的票据必须验签失败，而不是拿着改过的 object key 去写。
func TestMultipartTicketTamperingIsRejected(t *testing.T) {
	svc := newMultipartService(t, nil)
	ctx := context.Background()
	mp := svc.Manager.(manager.MultipartUploader)
	session := initSession(t, svc.Service, "movie.bin", 1024)

	tampered := session.Ticket[:len(session.Ticket)-2] + "ff"
	_, err := mp.UploadPart(ctx, tampered, 1, bytes.NewReader([]byte("x")), 1, nil)
	if !errors.Is(err, manager.ErrMultipartTicketInvalid) {
		t.Fatalf("err = %v, want ErrMultipartTicketInvalid", err)
	}
}

// Abort 之后分片必须消失，否则放弃的上传会一直占着空间计费。
func TestMultipartAbortClearsStaging(t *testing.T) {
	svc := newMultipartService(t, nil)
	ctx := context.Background()
	mp := svc.Manager.(manager.MultipartUploader)
	session := initSession(t, svc.Service, "movie.bin", 1024)

	if _, err := mp.UploadPart(ctx, session.Ticket, 1,
		bytes.NewReader(bytes.Repeat([]byte("a"), 2048)), 2048, nil); err != nil {
		t.Fatalf("upload part: %v", err)
	}
	if err := mp.AbortMultipartUpload(ctx, session.Ticket, nil); err != nil {
		t.Fatalf("abort: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(svc.root, ".multipart"))
	if len(entries) != 0 {
		t.Errorf("abort left %d sessions behind", len(entries))
	}
}

// 关键：正在进行的分片不能被孤儿对账当成孤儿删掉。
func TestReconcileSkipsMultipartStaging(t *testing.T) {
	svc := newMultipartService(t, nil)
	ctx := context.Background()
	mp := svc.Manager.(manager.MultipartUploader)
	session := initSession(t, svc.Service, "movie.bin", 1<<20)

	if _, err := mp.UploadPart(ctx, session.Ticket, 1,
		bytes.NewReader(bytes.Repeat([]byte("a"), 4096)), 4096, nil); err != nil {
		t.Fatalf("upload part: %v", err)
	}
	// 把分片的时间推到安全窗口之外，让它成为对账的候选。
	stagingDir := filepath.Join(svc.root, ".multipart")
	old := time.Now().Add(-72 * time.Hour)
	_ = filepath.Walk(stagingDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			_ = os.Chtimes(path, old, old)
		}
		return nil
	})

	mgr := svc.Manager.(*manager.DefaultManager)
	result, err := mgr.ReconcileOrphans(ctx, &manager.ReconcileOptions{
		Before: time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Deleted != 0 {
		t.Fatalf("orphan sweep deleted %d in-flight parts", result.Deleted)
	}
	// 分片还在，上传还能接着完成。
	if _, err = os.Stat(filepath.Join(stagingDir, session.UploadID)); err != nil {
		t.Errorf("in-flight multipart session was destroyed: %v", err)
	}
}

// 没开开关时四条路由不注册，Manager 也直接拒绝。
func TestMultipartDisabledByDefault(t *testing.T) {
	svc := newServiceWithRoot(t, nil)
	mp := svc.Manager.(manager.MultipartUploader)
	_, err := mp.InitMultipartUpload(context.Background(), &model.MultipartInitRequest{
		FileName: "movie.bin",
		BizType:  "video",
	})
	if !errors.Is(err, manager.ErrMultipartDisabled) {
		t.Fatalf("err = %v, want ErrMultipartDisabled", err)
	}
}

// uploadID 会被拼进文件路径，构造出来的值不能逃出暂存目录。
func TestLocalMultipartRejectsHostileUploadID(t *testing.T) {
	root := t.TempDir()
	p, err := local.NewLocalProvider(config.ProviderConfig{
		RootDir: root, Domain: "cdn.test", Scheme: "https",
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	_, err = p.UploadPart(context.Background(), "a/b.bin", "../../etc", 1,
		strings.NewReader("x"), 1)
	if !errors.Is(err, provider.ErrUploadSessionInvalid) {
		t.Fatalf("err = %v, want ErrUploadSessionInvalid", err)
	}
}
