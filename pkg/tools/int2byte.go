package tools

import (
	"bytes"
	"encoding/binary"
)

// byte其实是uint8的别名，byte 和 uint8 之间可以直接进行互转
// 如果需要将int32转成byte类型，我们只需要一个长度为4的[]byte数组就可以了
func Int2Bytes(a int) []byte {
	x := int32(a)
	// 构造字节容器
	buf := bytes.NewBuffer(make([]byte, 4)) // io接口
	binary.Write(buf, binary.BigEndian, x)
	return buf.Bytes()
}

func Bytes2Int(b []byte) int {
	// 因为传入的只是一个字节数组，并不满足binary的格式编码化，所以需要转为io接口
	buf := bytes.NewBuffer(b)
	var x int32
	binary.Read(buf, binary.BigEndian, &x)
	return int(x)
}
