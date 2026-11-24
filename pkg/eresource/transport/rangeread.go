package transport

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// errRangeNotSatisfiable Range 请求的区间落在对象之外，对应 416。
var errRangeNotSatisfiable = errors.New("eresource: requested range is not satisfiable")

// byteRange 一次 Range 请求解析出的区间。
type byteRange struct {
	offset int64 // 起始字节（含）
	length int64 // 字节数，必然 > 0
}

// last 返回区间的末字节下标（含），用于拼 Content-Range。
func (r byteRange) last() int64 { return r.offset + r.length - 1 }

// contentRange 返回 "bytes 0-499/1234" 形式的响应头值。
func (r byteRange) contentRange(size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.offset, r.last(), size)
}

// parseByteRange 解析 Range 请求头。
//
// 三个返回值分别是：区间、是否需要按区间响应、错误。
// ok 为 false 且 err 为 nil 表示「按整个对象响应」——HTTP 明确允许服务端
// 忽略 Range，因此下列情况一律走这条路而不是报错：
//
//   - 没有 Range 头，或单位不是 bytes
//   - 多区间请求（multipart/byteranges 响应体的复杂度远超它的收益，
//     而客户端拿到 200 全量响应同样能正常工作）
//   - 语法不合法（按 RFC 9110，不合法的 Range 应当被忽略而不是报错）
//
// 只有「语法合法但区间整个落在对象之外」才返回 errRangeNotSatisfiable，
// 这是 RFC 要求必须回 416 的唯一情形。
func parseByteRange(header string, size int64) (byteRange, bool, error) {
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found {
		return byteRange{}, false, nil
	}
	if strings.Contains(spec, ",") {
		return byteRange{}, false, nil
	}
	start, end, found := strings.Cut(spec, "-")
	if !found {
		return byteRange{}, false, nil
	}
	start, end = strings.TrimSpace(start), strings.TrimSpace(end)

	// size 未知（部分 provider 的 Stat 给不出大小）时无法换算后缀区间，
	// 也无法判定可满足性，直接退回整体响应。
	if size <= 0 {
		return byteRange{}, false, nil
	}

	// "bytes=-500"：最后 500 字节。
	if start == "" {
		suffix, err := strconv.ParseInt(end, 10, 64)
		if err != nil || suffix <= 0 {
			return byteRange{}, false, nil
		}
		if suffix > size {
			suffix = size
		}
		return byteRange{offset: size - suffix, length: suffix}, true, nil
	}

	offset, err := strconv.ParseInt(start, 10, 64)
	if err != nil || offset < 0 {
		return byteRange{}, false, nil
	}
	if offset >= size {
		return byteRange{}, false, errRangeNotSatisfiable
	}

	// "bytes=500-"：从 500 读到末尾。
	if end == "" {
		return byteRange{offset: offset, length: size - offset}, true, nil
	}

	last, err := strconv.ParseInt(end, 10, 64)
	if err != nil || last < offset {
		return byteRange{}, false, nil
	}
	if last >= size {
		last = size - 1
	}
	return byteRange{offset: offset, length: last - offset + 1}, true, nil
}
