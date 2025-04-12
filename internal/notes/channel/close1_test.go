package channel

import "testing"

// 直接关闭channel，在还有后续业务处理的情况
// 粗暴关闭channel
func TestCloseDirect(t *testing.T) {

}

func SafeClose(ch chan int) (justClosed bool) {
	defer func() {
		if recover() != nil {
			// The return result can be altered
			// in a defer function call.
			justClosed = false
		}
	}()

	// assume ch != nil here.
	close(ch) // panic if ch is closed
	// 如果本身是关闭的，那么这里就会panic
	return true // <=> justClosed = true; return
}

// 向潜在的用户发送值
func SafeSend(ch chan int, value int) (closed bool) {
	defer func() {
		if recover() != nil {
			closed = true
		}
	}()

	ch <- value // panic if ch is closed
	// 如果ch被接收方或者另外一个线程关闭了，那么这里就会panic
	return false // <=> closed = false; return
}
