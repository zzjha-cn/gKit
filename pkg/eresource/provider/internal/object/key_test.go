package object

import (
	"errors"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

func TestStrictSegment(t *testing.T) {
	ok := []string{"", "crm", "acct-1", "Tenant_9", "a"}
	for _, raw := range ok {
		got, err := strictSegment("account_id", raw)
		if err != nil {
			t.Errorf("strictSegment(%q) unexpected error: %v", raw, err)
		}
		if got != raw {
			t.Errorf("strictSegment(%q) = %q, want it returned verbatim", raw, got)
		}
	}

	bad := []string{"a.b", "a/b", "../etc", "a b", "acct:1", "中文", strings.Repeat("x", MaxSegmentLength+1)}
	for _, raw := range bad {
		if _, err := strictSegment("account_id", raw); !errors.Is(err, ErrInvalidSegment) {
			t.Errorf("strictSegment(%q) err = %v, want ErrInvalidSegment", raw, err)
		}
	}

	// 核心动机：静默剥离会让不同的租户折叠成同一个路径段
	if got := safeSegment("a.b"); got != "ab" {
		t.Fatalf("precondition changed: safeSegment(%q) = %q", "a.b", got)
	}
}

// 两个 Provider 的身份前缀生成必须一致。
func TestIdentitySegments(t *testing.T) {
	parts, err := IdentitySegments(&model.UploadRequest{Namespace: "crm", AccountID: "acct-1", BizType: "avatar"})
	if err != nil {
		t.Fatalf("IdentitySegments: %v", err)
	}
	if strings.Join(parts, "/") != "crm/acct-1/avatar" {
		t.Errorf("parts = %v", parts)
	}

	// 空的租户层级被跳过，biz_type 缺省为 default
	if parts, err = IdentitySegments(&model.UploadRequest{}); err != nil {
		t.Fatalf("IdentitySegments: %v", err)
	} else if strings.Join(parts, "/") != "default" {
		t.Errorf("parts = %v, want [default]", parts)
	}

	if _, err = IdentitySegments(&model.UploadRequest{Namespace: "a/b"}); !errors.Is(err, ErrInvalidSegment) {
		t.Errorf("err = %v, want ErrInvalidSegment", err)
	}
}

// 对象 ACL 必须逐对象跟随访问策略：bucket 继承在两种配法下都是错的。
// base_path 为空时不能在 object key 里留下空段（"/crm/..." 这种前导斜杠）。
func TestEmptyBasePathLeavesNoEmptySegment(t *testing.T) {
	for _, basePath := range []string{"", "/", "//"} {
		if got := PrefixKey(basePath, "crm/acct-a/doc/x.png"); got != "crm/acct-a/doc/x.png" {
			t.Errorf("base_path = %q 时 key = %q，期望不加任何前缀", basePath, got)
		}
	}
	if got := PrefixKey("/static/", "crm/x.png"); got != "static/crm/x.png" {
		t.Errorf("base_path 两侧斜杠未被归一：%q", got)
	}
}
