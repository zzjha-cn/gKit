package aliyunoss

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/config"
	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// PostObject 直传表单的字段名与常量。
//
// 用 V4（OSS4-HMAC-SHA256）而不是老的 V1 签名：本 Provider 的客户端本身
// 就是按 oss.AuthV4 构造的，两边保持一致，避免同一个 bucket 上出现
// 两套签名体系（V1 在部分新建地域已经不再可用）。
const (
	postFieldKey          = "key"
	postFieldPolicy       = "policy"
	postFieldSignatureVer = "x-oss-signature-version"
	postFieldCredential   = "x-oss-credential"
	postFieldDate         = "x-oss-date"
	postFieldSignature    = "x-oss-signature"
	postFieldObjectACL    = "x-oss-object-acl"
	// 静态加密同样要写进 policy 的 conditions：直传的字节服务端看不到，
	// 凭证里没锁死加密方式的话，客户端可以不带这个表单字段，
	// 对象就以明文落盘——而服务端这边一切正常，没有任何信号。
	postFieldSSE             = "x-oss-server-side-encryption"
	postFieldSSEKeyID        = "x-oss-server-side-encryption-key-id"
	postFieldContentType     = "Content-Type"
	postFieldSuccessStatus   = "success_action_status"
	postFieldFile            = "file"
	ossV4SignatureVersion    = "OSS4-HMAC-SHA256"
	ossV4RequestType         = "aliyun_v4_request"
	ossV4SigningKeyPrefix    = "aliyun_v4"
	ossV4Product             = "oss"
	postPolicyTimeLayout     = "20060102T150405Z"
	postPolicyDateLayout     = "20060102"
	postPolicyExpireLayout   = "2006-01-02T15:04:05.000Z"
	postSuccessActionsStatus = "200"
)

// SignUploadPolicy 签发 OSS PostObject 直传表单凭证，实现 DirectUploader。
//
// ctx 仅为满足接口形态而保留：签名是纯本地计算，不发起任何网络请求。
//
// 凭证的全部约束都写在 policy 文档的 conditions 里，由 OSS 在接收表单时校验：
//   - ["eq", "$key", objectKey]        对象路径精确锁定，客户端不能改写到别的租户目录
//   - ["eq", "$Content-Type", ct]      声明类型锁定，防止拿到图片凭证后传 HTML
//   - ["content-length-range", 1, max] 体积上限由 OSS 执行，空文件也一并拒掉
//   - x-oss-object-acl                 ACL 跟随访问策略，不继承 bucket 默认值
//
// 这些条件缺一不可：少了 key 的 eq，客户端可以用通配前缀把对象写到任意位置；
// 少了 content-length-range，一张凭证就能往 bucket 里灌任意大的文件。
func (p *AliOSSProvider) SignUploadPolicy(ctx context.Context, req *model.DirectUploadRequest) (*model.DirectUploadPolicy, error) {
	if req == nil {
		return nil, fmt.Errorf("direct upload request is nil")
	}
	if req.MaxSize <= 0 {
		return nil, fmt.Errorf("direct upload max size must be positive")
	}
	if req.ExpireSeconds <= 0 {
		return nil, fmt.Errorf("direct upload expire seconds must be positive")
	}

	// object key 复用普通上传那套生成规则（租户前缀 + 日期分区 + 随机名），
	// 直传与中转上传因此落在同一套物理布局里，运维侧不必分两种路径看。
	objectKey, err := p.objectKey(&model.UploadRequest{
		Namespace: req.Namespace,
		AccountID: req.AccountID,
		BizType:   req.BizType,
		Ext:       req.Ext,
	})
	if err != nil {
		return nil, err
	}

	contentType := req.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	acl := string(objectACL(req.AccessPolicy))

	now := time.Now().UTC()
	expireAt := now.Add(time.Duration(req.ExpireSeconds) * time.Second)
	date := now.Format(postPolicyDateLayout)
	signTime := now.Format(postPolicyTimeLayout)
	credential := fmt.Sprintf("%s/%s/%s/%s/%s", p.accessKey, date, p.region, ossV4Product, ossV4RequestType)

	policyDoc := map[string]any{
		"expiration": expireAt.Format(postPolicyExpireLayout),
		"conditions": []any{
			map[string]string{"bucket": p.bucketName},
			map[string]string{postFieldSignatureVer: ossV4SignatureVersion},
			map[string]string{postFieldCredential: credential},
			map[string]string{postFieldDate: signTime},
			map[string]string{postFieldObjectACL: acl},
			map[string]string{"x-oss-forbid-overwrite": "true"},
			[]any{"eq", "$" + postFieldKey, objectKey},
			[]any{"eq", "$" + postFieldContentType, contentType},
			[]any{"content-length-range", 1, req.MaxSize},
		},
	}
	conditions := policyDoc["conditions"].([]any)
	sseFields := map[string]string{}
	if p.sseMode != "" {
		conditions = append(conditions, map[string]string{postFieldSSE: p.sseMode})
		sseFields[postFieldSSE] = p.sseMode
		if p.sseMode == config.SSEModeKMS && p.sseKeyID != "" {
			conditions = append(conditions, map[string]string{postFieldSSEKeyID: p.sseKeyID})
			sseFields[postFieldSSEKeyID] = p.sseKeyID
		}
		policyDoc["conditions"] = conditions
	}
	raw, err := json.Marshal(policyDoc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal post policy: %w", err)
	}
	encodedPolicy := base64.StdEncoding.EncodeToString(raw)
	signature := hex.EncodeToString(hmacSHA256(ossV4SigningKey(p.secretKey, date, p.region), []byte(encodedPolicy)))

	host := p.domain
	if host == "" {
		host = fmt.Sprintf("%s.%s", p.bucketName, p.endpoint)
	}

	formFields := map[string]string{
		postFieldKey:             objectKey,
		postFieldPolicy:          encodedPolicy,
		postFieldSignatureVer:    ossV4SignatureVersion,
		postFieldCredential:      credential,
		postFieldDate:            signTime,
		postFieldSignature:       signature,
		postFieldObjectACL:       acl,
		"x-oss-forbid-overwrite": "true",
		postFieldContentType:     contentType,
		postFieldSuccessStatus:   postSuccessActionsStatus,
	}
	for k, v := range sseFields {
		formFields[k] = v
	}

	return &model.DirectUploadPolicy{
		Provider:   p.Name(),
		Method:     "POST",
		UploadURL:  fmt.Sprintf("%s://%s/", p.scheme, host),
		FileField:  postFieldFile,
		FormFields: formFields,
		ObjectKey:  objectKey,
		MaxSize:    req.MaxSize,
		ExpireAt:   expireAt,
		Bucket:     p.bucketName,
		// 与 Upload 返回的 StorePath 保持同一种写法，
		// 直传登记出来的元数据才能和中转上传的记录长得一样。
		StorePath: fmt.Sprintf("%s.%s/%s", p.bucketName, p.endpoint, objectKey),
	}, nil
}

// ossV4SigningKey 按阿里云 V4 规范派生签名密钥：
// HMAC 链 aliyun_v4{SK} → date → region → oss → aliyun_v4_request。
// 逐级派生的意义是最终密钥天然限定在「某天、某地域、某产品」范围内，
// 即使泄露也无法跨日期或跨地域复用。
func ossV4SigningKey(secretKey, date, region string) []byte {
	key := hmacSHA256([]byte(ossV4SigningKeyPrefix+secretKey), []byte(date))
	key = hmacSHA256(key, []byte(region))
	key = hmacSHA256(key, []byte(ossV4Product))
	return hmacSHA256(key, []byte(ossV4RequestType))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}
