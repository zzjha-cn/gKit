package tools

import (
	"math/rand"
	"strconv"
	"time"
)

// 随机字符串序列
var seedString = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

const (
	// 通过 6 bit 来推断出一个下标值 此长度的选择应该与seedString的长度自适应
	letterIdBits = 6

	// 将1左移letterIdBits位，如果6位，得到1000000，再减去1得到111111
	letterIdMask = 1<<letterIdBits - 1 // 2^6-1 = 63
	// 最大长度
	letterIdMax = 63 / letterIdBits // 63/6 = 10
)

// GenerateIdWithTime 结合时间生成id
func GenerateIdWithTime() string {
	ts := time.Now().Unix()
	str := GenerateString(14)
	s := strconv.Itoa(int(ts))

	return s + str
}

// GenerateString 生成随机字符串(较快速)
func GenerateString(n int) string {
	b := make([]byte, n, n)

	/* 通过随机数，构建一个下标值，通过这个下标获取到seedString中对应的符号，然后不断迭代组合，直到长度符合 */
	// src.Int63() 获取一串 63 bit 的随机数字(int64)
	// 期望从随机的一串数字中，按照每 6 bit (letterIdBits)切割并计算出一个int下标值，这串数据则可以切割letterIdMax次
	// 这个随机出来的int64，可以供letterIdMask使用letterIdMax次。
	// 通过&运算得到一个int（这个int小于letterIdMask），以int为下标拿到字符串中的值

	for i, cache, remain := n-1, rand.Int63(), letterIdMax; i >= 0; {
		if remain == 0 {
			cache, remain = rand.Int63(), letterIdMax
		}
		if idx := int(cache & letterIdMask); idx < len(seedString) {
			b[i] = seedString[idx]
			i--
		}
		cache >>= letterIdBits
		remain--
	}
	return string(b)
}
