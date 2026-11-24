package local

import (
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

func TestBasePathIsAStoragePrefix(t *testing.T) {
	p := &LocalProvider{basePath: "static"}
	req := &model.UploadRequest{
		Namespace: "crm", AccountID: "acct-a", BizType: "doc",
		Hash: "d41d8cd98f00b204e9800998ecf8427e", Ext: ".png",
	}
	key, err := p.objectKey(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "static/crm/acct-a/doc/") {
		t.Fatalf("object key = %q, want storage prefix", key)
	}
	link, err := p.BuildURL(key, &model.URLBuildOption{Scheme: "https", Host: "cdn.test"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(link, "/static/") != 1 || !strings.HasPrefix(link, "https://cdn.test/static/crm/acct-a/doc/") {
		t.Errorf("URL = %q, want storage prefix exactly once", link)
	}
}
