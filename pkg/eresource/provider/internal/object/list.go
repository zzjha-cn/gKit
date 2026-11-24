package object

// defaultListLimit 单页默认条数。
const defaultListLimit = 200

// maxListLimit 单页上限。OSS 的 ListObjects 硬上限就是 1000，
// 本地实现跟着对齐，免得同一段对账代码在两种 provider 下页大小差一个数量级。
const maxListLimit = 1000

func NormalizeListLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
}
