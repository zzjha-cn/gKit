package jsonstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
)

func TestJSONStoreRollsBackFailedWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "resources.json")
	st, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, &model.Resource{ID: "existing", Name: "original"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}

	if err := st.Create(ctx, &model.Resource{ID: "new"}); err == nil {
		t.Fatal("create succeeded despite failed write")
	}
	if _, err := st.GetByID(ctx, "new"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed create was visible: %v", err)
	}
	name := "changed"
	mutations := []func() error{
		func() error { return st.Update(ctx, &model.Resource{ID: "existing", Name: name}) },
		func() error { return st.Patch(ctx, "existing", &model.ResourcePatch{Name: &name}) },
		func() error { return st.Delete(ctx, "existing") },
	}
	for _, mutate := range mutations {
		if err := mutate(); err == nil {
			t.Fatal("mutation succeeded despite failed write")
		}
		resource, err := st.GetByID(ctx, "existing")
		if err != nil || resource.Name != "original" {
			t.Fatalf("failed mutation changed state: %+v, %v", resource, err)
		}
	}
}
