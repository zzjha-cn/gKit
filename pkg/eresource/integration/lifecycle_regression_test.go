package integration_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

func TestTenantOperationsRequireCompleteIdentity(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)
	existing := uploadForTenant(t, svc, "crm", "account", "alice")
	mgr := svc.Manager.(*manager.DefaultManager)
	for _, identity := range [][2]string{{"", ""}, {"crm", ""}, {"", "account"}} {
		namespace, account := identity[0], identity[1]
		if _, err := mgr.PurgeTenant(ctx, namespace, account, namespace+"/"+account); !errors.Is(err, manager.ErrTenantIdentityRequired) {
			t.Fatalf("purge with %q returned %v", identity, err)
		}
		err := mgr.ExportTenant(ctx, namespace, account, func(_ []*model.Resource) error {
			t.Fatal("export opened with incomplete tenant identity")
			return nil
		})
		if !errors.Is(err, manager.ErrTenantIdentityRequired) {
			t.Fatalf("export with %q returned %v", identity, err)
		}
	}
	if _, err := svc.Store.GetByID(ctx, existing.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDirectCompletionChecksQuotaAndReusesResult(t *testing.T) {
	ctx := context.Background()
	svc, p := newDirectService(t, func(cfg *config.Config) {
		cfg.Manager.EnableQuota = true
		cfg.Manager.QuotaMaxBytes = 4
	})
	uploader := directManager(t, svc)
	policy, err := uploader.CreateDirectUpload(ctx, directRequest("crm", "account", "alice", "file.bin", "application/octet-stream"))
	if err != nil {
		t.Fatal(err)
	}
	req := &model.DirectUploadCompleteRequest{Ticket: policy.Ticket, Namespace: "crm", AccountID: "account", Creator: "alice"}
	p.putDirect(policy.ObjectKey, []byte("large"))
	if _, err := uploader.CompleteDirectUpload(ctx, req); !errors.Is(err, manager.ErrQuotaExceeded) {
		t.Fatalf("oversized completion returned %v", err)
	}
	p.putDirect(policy.ObjectKey, []byte("fits"))
	first, err := uploader.CompleteDirectUpload(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := uploader.CompleteDirectUpload(ctx, req)
	if err != nil || retry.Resource.ID != first.Resource.ID || retry.Uploaded {
		t.Fatalf("retry returned %+v, %v", retry, err)
	}
	usage, err := svc.Manager.Usage(ctx, "crm", "account")
	if err != nil || usage.Files != 1 || usage.Bytes != 4 {
		t.Fatalf("usage after retry: %+v, %v", usage, err)
	}
}

func TestMultipartCompletionChecksActualQuota(t *testing.T) {
	ctx := context.Background()
	svc := newMultipartService(t, func(cfg *config.Config) {
		cfg.Manager.EnableQuota = true
		cfg.Manager.QuotaMaxBytes = 4
	})
	mp := svc.Manager.(manager.MultipartUploader)
	session := initSession(t, svc.Service, "file.bin", 1)
	body := []byte("large")
	part, err := mp.UploadPart(ctx, session.Ticket, 1, bytes.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mp.CompleteMultipartUpload(ctx, &model.MultipartCompleteRequest{
		Ticket: session.Ticket,
		Parts:  []model.MultipartPart{*part},
	}); !errors.Is(err, manager.ErrQuotaExceeded) {
		t.Fatalf("completion returned %v", err)
	}
}

func TestPurgeSelectsExpiredRecordsBeforeApplyingLimit(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, func(cfg *config.Config) { cfg.Access.SoftDelete = true })
	mgr := svc.Manager.(*manager.DefaultManager)
	old := upload(t, svc, "old.txt", []byte("old")).Resource
	newer := upload(t, svc, "new.txt", []byte("new")).Resource
	for _, resource := range []*model.Resource{old, newer} {
		if err := mgr.Delete(ctx, resource.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(-time.Hour)
	deletedAt := cutoff.Add(-time.Hour)
	if err := svc.Store.Patch(ctx, old.ID, &model.ResourcePatch{DeletedAt: &deletedAt}); err != nil {
		t.Fatal(err)
	}
	if count, err := mgr.PurgeDeleted(ctx, cutoff, 1); err != nil || count != 1 {
		t.Fatalf("purge returned %d, %v", count, err)
	}
	if _, err := svc.Store.GetByID(ctx, old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired record was not purged: %v", err)
	}
	if _, err := svc.Store.GetByID(ctx, newer.ID); err != nil {
		t.Fatalf("unexpired record disappeared: %v", err)
	}
}

func TestSoftDeleteRevokesGatewaySignedDownload(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, func(cfg *config.Config) {
		cfg.Access.SoftDelete = true
		cfg.Manager.DefaultAccessPolicy = model.AccessPolicyProtected
	})
	router := newTestRouter(t, svc)
	uploaded := upload(t, svc, "file.txt", []byte("example"))
	signed, err := url.Parse(uploaded.URL)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func() int {
		return serve(router, httptest.NewRequest(http.MethodGet, signed.RequestURI(), nil)).Code
	}
	if status := fetch(); status != http.StatusOK {
		t.Fatalf("before delete: status %d", status)
	}
	if err := svc.Manager.Delete(ctx, uploaded.Resource.ID, nil); err != nil {
		t.Fatal(err)
	}
	if status := fetch(); status != http.StatusNotFound {
		t.Fatalf("after delete: status %d", status)
	}
}

func TestMultipartRetriesRegistrationAfterMerge(t *testing.T) {
	ctx := context.Background()
	svc := newMultipartService(t, nil)
	mp := svc.Manager.(manager.MultipartUploader)
	body := []byte("content")
	session := initSession(t, svc.Service, "file.bin", int64(len(body)))
	part, err := mp.UploadPart(ctx, session.Ticket, 1, bytes.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &model.MultipartCompleteRequest{Ticket: session.Ticket, Parts: []model.MultipartPart{*part}}
	temporaryPath := svc.Config.Store.JSONFile + ".tmp"
	if err := os.Mkdir(temporaryPath, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.CompleteMultipartUpload(ctx, req); err == nil {
		t.Fatal("registration should have failed")
	}
	if err := os.Remove(temporaryPath); err != nil {
		t.Fatal(err)
	}
	first, err := mp.CompleteMultipartUpload(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := mp.CompleteMultipartUpload(ctx, req)
	if err != nil || retry.Resource.ID != first.Resource.ID || retry.Uploaded {
		t.Fatalf("retry returned %+v, %v", retry, err)
	}
}
