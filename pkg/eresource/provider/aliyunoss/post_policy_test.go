package aliyunoss

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

func newPostTestProvider(t *testing.T) *AliOSSProvider {
	t.Helper()
	p, err := NewAliOSSProvider(config.ProviderConfig{
		Bucket: "unit-bucket", Endpoint: "oss-cn-hangzhou.aliyuncs.com", Region: "cn-hangzhou",
		AccessKey: "unit-ak", SecretKey: "unit-sk", Scheme: "https",
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return p
}

// decodePolicyConditions 还原 policy 文档里的条件列表。
func decodePolicyConditions(t *testing.T, encoded string) []any {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	var doc struct {
		Expiration string `json:"expiration"`
		Conditions []any  `json:"conditions"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal policy: %v", err)
	}
	if doc.Expiration == "" {
		t.Error("policy 没有 expiration，凭证将永不过期")
	}
	return doc.Conditions
}

// 凭证的全部约束都靠 policy 条件执行：少一条就多一个客户端可以自己决定的维度。
func TestSignUploadPolicyLocksEveryCondition(t *testing.T) {
	p := newPostTestProvider(t)
	policy, err := p.SignUploadPolicy(context.Background(), &model.DirectUploadRequest{
		Namespace: "crm", AccountID: "acct-a", BizType: "image",
		Ext: ".png", ContentType: "image/png", AccessPolicy: model.AccessPolicyPrivate,
		MaxSize: 1024, ExpireSeconds: 900,
	})
	if err != nil {
		t.Fatalf("sign upload policy: %v", err)
	}

	if !strings.HasPrefix(policy.ObjectKey, "crm/acct-a/image/") || !strings.HasSuffix(policy.ObjectKey, ".png") {
		t.Errorf("object key = %q，应当落在租户前缀下", policy.ObjectKey)
	}
	if policy.UploadURL != "https://unit-bucket.oss-cn-hangzhou.aliyuncs.com/" {
		t.Errorf("upload url = %q", policy.UploadURL)
	}
	if policy.FormFields[postFieldObjectACL] != "private" {
		t.Errorf("object acl = %q，非 public 资源必须落到 private", policy.FormFields[postFieldObjectACL])
	}
	if policy.FormFields[postFieldKey] != policy.ObjectKey {
		t.Errorf("表单的 key 字段与 object key 不一致")
	}

	conditions := decodePolicyConditions(t, policy.FormFields[postFieldPolicy])
	found := map[string]bool{}
	for _, condition := range conditions {
		switch value := condition.(type) {
		case map[string]any:
			// {"bucket": "..."} 这类单键条件
			for k := range value {
				found[k] = true
			}
		case []any:
			// ["eq", "$key", "..."] 这类三元条件，只记前两项
			if len(value) >= 2 {
				op, _ := value[0].(string)
				field, _ := value[1].(string)
				found[op+":"+field] = true
			}
		}
	}
	for _, want := range []string{
		"bucket", postFieldSignatureVer, postFieldCredential, postFieldDate, postFieldObjectACL,
		"eq:$" + postFieldKey, "eq:$" + postFieldContentType,
	} {
		if !found[want] {
			t.Errorf("policy 缺少条件 %q: %v", want, conditions)
		}
	}
	if !hasContentLengthRange(conditions, 1024) {
		t.Errorf("policy 缺少 content-length-range 上限，一张凭证就能灌任意大的文件: %v", conditions)
	}
}

func hasContentLengthRange(conditions []any, max float64) bool {
	for _, condition := range conditions {
		list, ok := condition.([]any)
		if !ok || len(list) != 3 {
			continue
		}
		if name, _ := list[0].(string); name != "content-length-range" {
			continue
		}
		upper, _ := list[2].(float64)
		return upper == max
	}
	return false
}

// public 资源的对象 ACL 必须跟着策略走，否则直传上去的图在 OSS 侧取不到。
func TestSignUploadPolicyPublicACL(t *testing.T) {
	p := newPostTestProvider(t)
	policy, err := p.SignUploadPolicy(context.Background(), &model.DirectUploadRequest{
		Namespace: "crm", AccountID: "acct-a", Ext: ".png", ContentType: "image/png",
		AccessPolicy: model.AccessPolicyPublic, MaxSize: 1024, ExpireSeconds: 900,
	})
	if err != nil {
		t.Fatalf("sign upload policy: %v", err)
	}
	if policy.FormFields[postFieldObjectACL] != "public-read" {
		t.Errorf("object acl = %q, want public-read", policy.FormFields[postFieldObjectACL])
	}
}

// 签名必须是对 base64(policy) 做 V4 派生密钥的 HMAC，
// 这是 OSS 侧唯一会复算的值，算错了直传会在客户端那边才暴露。
func TestSignUploadPolicySignatureMatchesV4Chain(t *testing.T) {
	p := newPostTestProvider(t)
	policy, err := p.SignUploadPolicy(context.Background(), &model.DirectUploadRequest{
		Namespace: "crm", AccountID: "acct-a", Ext: ".png", ContentType: "image/png",
		AccessPolicy: model.AccessPolicyPrivate, MaxSize: 1024, ExpireSeconds: 900,
	})
	if err != nil {
		t.Fatalf("sign upload policy: %v", err)
	}
	credential := policy.FormFields[postFieldCredential]
	parts := strings.Split(credential, "/")
	if len(parts) != 5 || parts[0] != "unit-ak" || parts[2] != "cn-hangzhou" ||
		parts[3] != ossV4Product || parts[4] != ossV4RequestType {
		t.Fatalf("credential = %q 不符合 {ak}/{date}/{region}/oss/aliyun_v4_request", credential)
	}
	want := hex.EncodeToString(hmacSHA256(
		ossV4SigningKey("unit-sk", parts[1], "cn-hangzhou"),
		[]byte(policy.FormFields[postFieldPolicy]),
	))
	if policy.FormFields[postFieldSignature] != want {
		t.Errorf("signature = %q, want %q", policy.FormFields[postFieldSignature], want)
	}
}

// 没有体积上限或没有有效期的凭证一律拒签。
func TestSignUploadPolicyRejectsUnboundedRequests(t *testing.T) {
	p := newPostTestProvider(t)
	cases := map[string]*model.DirectUploadRequest{
		"没有体积上限": {MaxSize: 0, ExpireSeconds: 900},
		"没有有效期":  {MaxSize: 1024, ExpireSeconds: 0},
		"空请求":    nil,
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := p.SignUploadPolicy(context.Background(), req); err == nil {
				t.Fatal("期望拒签，实际签发成功")
			}
		})
	}
}
