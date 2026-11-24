package object

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
)

// IdentitySegments 生成对象路径中的身份前缀：{namespace}/{account_id}/{biz_type}。
// namespace 与 account_id 为空时对应层级会被跳过，保持向下兼容。
//
// 这些字段既决定物理隔离边界，也参与去重判定，因此必须走 strictSegment 严格校验：
// 静默剥离非法字符会让不同的输入折叠成同一个路径段，隔离边界在无人察觉时消失。
func IdentitySegments(req *model.UploadRequest) ([]string, error) {
	var parts []string
	for _, field := range []struct {
		name  string
		value string
	}{
		{"namespace", req.Namespace},
		{"account_id", req.AccountID},
	} {
		segment, err := strictSegment(field.name, field.value)
		if err != nil {
			return nil, err
		}
		if segment != "" {
			parts = append(parts, segment)
		}
	}
	bizType, err := strictSegment("biz_type", req.BizType)
	if err != nil {
		return nil, err
	}
	if bizType == "" {
		bizType = "default"
	}
	return append(parts, bizType), nil
}

// PrefixKey 把 base_path 拼到 object key 前面。
//
// base_path 的语义是**存储前缀**：它是 object key 的一部分，会随元数据持久化，
// 而不是只在拼 URL 时临时加上的路径段。这个方向不是二选一——对象存储的
// URL 路径就是 object key 本身，如果 base_path 只当 URL 前缀，
// 拼出来的地址根本指不到真实对象。local 因此向对象存储看齐。
func PrefixKey(basePath, key string) string {
	basePath = strings.Trim(basePath, "/")
	if basePath == "" {
		return key
	}
	return basePath + "/" + key
}

// Name 生成 object key 的文件名部分：{hash}-{random}{ext}。
//
// 随机后缀是必须的。只用内容哈希的话，同一份内容的多次上传会落到同一个
// object key，于是多条 Resource 记录共享同一个物理对象——删除其中任意一条
// 就会把文件删掉，剩下的记录全部变成 404。关闭去重（默认值）时这是必然发生的，
// 开启去重时 access_policy 不同的两次上传也会走到这里。
//
// 加随机后缀不会削弱秒传：去重查的是 Resource.Hash 字段而不是 object key，
// 代价只是同内容多副本时多占一份空间，换来「一条记录 ↔ 一个对象」的确定关系。
func Name(req *model.UploadRequest) (string, error) {
	suffix, err := RandomHex(8)
	if err != nil {
		return "", err
	}
	hash := safeSegment(req.Hash)
	if hash == "" {
		return suffix + normalizeExt(req.Ext), nil
	}
	return hash + "-" + suffix + normalizeExt(req.Ext), nil
}
func URL(scheme, host, basePath, objectKey string) string {
	segments := strings.Trim(objectKey, "/")
	if basePath != "" {
		segments = strings.Trim(basePath, "/") + "/" + segments
	}
	escaped := make([]string, 0, 8)
	for _, segment := range strings.Split(segments, "/") {
		escaped = append(escaped, url.PathEscape(segment))
	}
	return fmt.Sprintf("%s://%s/%s", scheme, host, strings.Join(escaped, "/"))
}

// MaxSegmentLength 单个路径段允许的最大长度。
const MaxSegmentLength = 64

// ErrInvalidSegment 表示某个用于拼接对象路径的字段含有非法字符或超长。
var ErrInvalidSegment = errors.New("eresource: invalid object path segment")

// safeSegment 只保留路径段中的安全字符，杜绝 ".." 与分隔符带来的路径穿越。
//
// 仅用于扩展名这类「非身份」字段：丢掉非法字符不会造成语义混淆。
// 租户、业务线等参与路径与去重判定的字段必须走 strictSegment。
func safeSegment(raw string) string {
	var builder strings.Builder
	for _, r := range raw {
		if isSegmentRune(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// strictSegment 校验一个身份类路径段，非法即报错，绝不静默剥离。
//
// safeSegment 的静默剥离会让不同的输入折叠成同一个路径段：
// account "a.b" 与 "ab" 都会变成 "ab"，两个租户的对象因此落进同一层目录，
// 隔离边界在无人察觉的情况下消失。这类字段宁可让调用方收到一个明确的错误。
func strictSegment(field, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > MaxSegmentLength {
		return "", fmt.Errorf("%w: %s exceeds %d characters", ErrInvalidSegment, field, MaxSegmentLength)
	}
	for _, r := range raw {
		if !isSegmentRune(r) {
			return "", fmt.Errorf("%w: %s contains %q; only letters, digits, '-' and '_' are allowed", ErrInvalidSegment, field, r)
		}
	}
	return raw, nil
}

// isSegmentRune 判断字符能否安全地出现在路径段中。
func isSegmentRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		return true
	default:
		return false
	}
}

// normalizeExt 归一化扩展名，返回值带前导点；非法扩展名直接丢弃。
func normalizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	ext = safeSegment(ext)
	if ext == "" {
		return ""
	}
	return "." + ext
}

func RandomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
