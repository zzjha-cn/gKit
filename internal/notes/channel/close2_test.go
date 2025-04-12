package channel

import (
	"sync"
	"testing"
)

func TestCloseNormal(t *testing.T) {

}

// sync.once 关闭——确保只会执行一次
type MyChannel struct {
	Ch   chan int
	once sync.Once
}

func NewChannel() *MyChannel {
	return &MyChannel{
		Ch: make(chan int),
	}
}

func (mc *MyChannel) SafeClose() {
	mc.once.Do(func() {
		close(mc.Ch)
	})
}

// 但是上面这个有点问题，就是可能多次执行的时候，会有多次关闭channel的情况

// 这个可以使用sync.mutex防止资源竞争
type MyChan2 struct {
	ch      chan int
	closeOr bool
	m       sync.Mutex
}

func NewChan2() *MyChan2 {
	return &MyChan2{
		ch:      make(chan int),
		closeOr: false,
	}
}

func (mc *MyChan2) SafeClose2() {
	mc.m.Lock()
	defer mc.m.Unlock()

	if !mc.closeOr {
		close(mc.ch)
		mc.closeOr = true
	}
}

func (mc *MyChan2) IsColse() bool {
	mc.m.Lock()
	defer mc.m.Unlock()
	return mc.closeOr
}
