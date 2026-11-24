package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/manager"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

func TestExportTenantIsScopedAndRedacted(t *testing.T) {
	svc := newTestService(t, nil)
	ctx := context.Background()
	uploadForTenant(t, svc, "crm", "acct-a", "u-1")
	uploadForTenant(t, svc, "crm", "acct-a", "u-1")
	uploadForTenant(t, svc, "crm", "acct-b", "u-2")

	mgr := svc.Manager.(*manager.DefaultManager)
	var exported []*model.Resource
	err := mgr.ExportTenant(ctx, "crm", "acct-a", func(page []*model.Resource) error {
		exported = append(exported, page...)
		return nil
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exported) != 2 {
		t.Fatalf("exported %d resources, want 2", len(exported))
	}
	for _, resource := range exported {
		if resource.AccountID != "acct-a" {
			t.Errorf("export leaked tenant %q", resource.AccountID)
		}
		if resource.ObjectKey != "" || resource.StorePath != "" {
			t.Errorf("export leaked the storage layout: %+v", resource)
		}
	}
}

func TestPurgeTenantRequiresConfirmation(t *testing.T) {
	svc := newTestService(t, nil)
	ctx := context.Background()
	uploadForTenant(t, svc, "crm", "acct-a", "u-1")

	mgr := svc.Manager.(*manager.DefaultManager)
	if _, err := mgr.PurgeTenant(ctx, "crm", "acct-a", "yes"); !errors.Is(err, manager.ErrPurgeNotConfirmed) {
		t.Fatalf("err = %v, want ErrPurgeNotConfirmed", err)
	}
	total, err := svc.Manager.Count(ctx, &model.Query{Namespace: "crm", AccountID: "acct-a"})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Fatalf("an unconfirmed purge removed data: count = %d", total)
	}
}

func TestPurgeTenantRemovesEverythingForThatTenantOnly(t *testing.T) {
	svc := newTestService(t, nil)
	ctx := context.Background()
	uploadForTenant(t, svc, "crm", "acct-a", "u-1")
	uploadForTenant(t, svc, "crm", "acct-a", "u-1")
	keeper := uploadForTenant(t, svc, "crm", "acct-b", "u-2")

	mgr := svc.Manager.(*manager.DefaultManager)
	purged, err := mgr.PurgeTenant(ctx, "crm", "acct-a", "crm/acct-a")
	if err != nil {
		t.Fatalf("purge tenant: %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged %d, want 2", purged)
	}
	gone, err := svc.Manager.Count(ctx, &model.Query{Namespace: "crm", AccountID: "acct-a"})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if gone != 0 {
		t.Errorf("tenant still has %d resources after purge", gone)
	}
	if _, err = svc.Manager.Get(ctx, keeper.ID, nil); err != nil {
		t.Errorf("another tenant's data was destroyed: %v", err)
	}
}
