package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// 签名 URL 的查询参数名
const (
	SignParamExpire = "t" // 过期时间戳（秒）
	SignParamUser   = "u" // 授权用户标识，可为空
	SignParamSign   = "s" // 防伪签名摘要
	// SignParamBind 标记该地址是否绑定到 SignParamUser 指定的用户。
	// 取值只有 "1"；不绑定时该参数不出现，也不参与摘要计算——
	// 这样未绑定地址的签名格式与引入绑定之前完全一致，老地址不会失效。
	SignParamBind = "b"

	// bindEnabled 是 SignParamBind 唯一被认可的取值。
	bindEnabled = "1"
)

var (
	ErrSignatureMissing = errors.New("eresource: signature is missing")
	ErrSignatureInvalid = errors.New("eresource: signature is invalid")
	ErrSignatureExpired = errors.New("eresource: signature is expired")
	// ErrIdentityRequired 绑定用户的地址缺少请求方身份。
	ErrIdentityRequired = errors.New("eresource: signed url is bound to a user but the requester is unidentified")
	// ErrIdentityMismatch 请求方不是该地址绑定的用户。
	ErrIdentityMismatch = errors.New("eresource: signed url is bound to a different user")
)

// SignObject 基于 HMAC-SHA256 计算对象访问签名。
// 相比裸 hash(message+secret)，HMAC 结构天然免疫长度扩展攻击。
//
// bindUser 为 true 时在被签消息末尾追加 "|b=1"，使「是否绑定用户」本身
// 也受签名保护：攻击者摘掉 URL 上的 b 参数会让消息退回不带后缀的形态，
// 摘要随之对不上，因此无法把绑定地址降级成可转发地址。
func SignObject(objectKey string, expireAt int64, userID string, bindUser bool, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%s|%d|%s", objectKey, expireAt, userID)
	if bindUser {
		_, _ = fmt.Fprintf(mac, "|%s=%s", SignParamBind, bindEnabled)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// SignedQuery 生成签名 URL 所需的查询参数集合。
//
// bindUser 要求 userID 非空：绑定到一个空身份的地址谁都匹配不上，
// 等于签发了一条永远用不了的链接，所以这里直接退化成不绑定。
// 调用方（Manager）负责保证需要绑定时 userID 一定存在。
func SignedQuery(objectKey string, expireAt int64, userID string, bindUser bool, secret string) url.Values {
	bindUser = bindUser && userID != ""
	query := url.Values{}
	query.Set(SignParamExpire, strconv.FormatInt(expireAt, 10))
	if userID != "" {
		query.Set(SignParamUser, userID)
	}
	if bindUser {
		query.Set(SignParamBind, bindEnabled)
	}
	query.Set(SignParamSign, SignObject(objectKey, expireAt, userID, bindUser, secret))
	return query
}

// VerifySignedQuery 校验签名摘要、用户绑定与有效期，任意一项不通过都返回对应错误。
//
// requesterID 是请求方的真实身份（由网关注入的会话信息），只在地址声明了
// 用户绑定时才参与校验；未绑定的地址仍然是 bearer token——谁持有谁能用。
func VerifySignedQuery(objectKey string, query url.Values, secret, requesterID string) error {
	got := query.Get(SignParamSign)
	if got == "" {
		return ErrSignatureMissing
	}
	expireAt, err := strconv.ParseInt(query.Get(SignParamExpire), 10, 64)
	if err != nil {
		return ErrSignatureInvalid
	}
	// 只认 "1"：其它取值一律按未绑定参与摘要计算，于是必然与签名对不上，
	// 不会出现「写了个 b=true 就被当成未绑定放行」的静默降级。
	bindUser := query.Get(SignParamBind) == bindEnabled
	signedUser := query.Get(SignParamUser)

	want := SignObject(objectKey, expireAt, signedUser, bindUser, secret)
	// 恒定时间比较，避免通过响应耗时侧信道逐字节爆破签名
	if !hmac.Equal([]byte(want), []byte(got)) {
		return ErrSignatureInvalid
	}
	// 身份校验排在过期判断之前：地址有没有过期，只告诉它真正的持有人。
	if bindUser {
		if requesterID == "" {
			return ErrIdentityRequired
		}
		if requesterID != signedUser {
			return ErrIdentityMismatch
		}
	}
	// 先验签再判过期：过期信息只对持有合法签名的请求方暴露
	if time.Now().Unix() > expireAt {
		return ErrSignatureExpired
	}
	return nil
}
