package wsx

import "time"

// ConnInfo 是暴露给观测 hook 的连接元信息，只读快照。
type ConnInfo struct {
	ID          string    // 进程内唯一连接 ID
	RemoteAddr  string    // 对端地址
	LocalAddr   string    // 本端地址
	Subprotocol string    // 协商后的子协议
	ConnectedAt time.Time // 连接建立时间
}

// Hooks 是观测挂载点，全部可选。
//
// 约定：hook 内不要做耗时操作。OnFrame 运行在读循环或写调用方的 goroutine 上，
// 阻塞它等于阻塞连接。
type Hooks struct {
	// OnConnect 在 Wrap 成功后调用。
	OnConnect func(info ConnInfo)
	// OnDisconnect 在连接收敛后调用，与 Handler.OnDisconnect 一样保证恰好一次。
	OnDisconnect func(info ConnInfo, d Disconnect)
	// OnFrame 每收发一帧调用一次，out 为 true 表示出站。
	// 建议按 Cause 分组统计断开数、按 out 分组统计收发字节。
	OnFrame func(info ConnInfo, messageType int, size int, out bool)
}

func (h Hooks) connect(info ConnInfo) {
	if h.OnConnect != nil {
		h.OnConnect(info)
	}
}

func (h Hooks) disconnect(info ConnInfo, d Disconnect) {
	if h.OnDisconnect != nil {
		h.OnDisconnect(info, d)
	}
}

func (h Hooks) frame(info ConnInfo, messageType, size int, out bool) {
	if h.OnFrame != nil {
		h.OnFrame(info, messageType, size, out)
	}
}
