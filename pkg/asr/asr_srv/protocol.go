package asrsrv

// 前端 ↔ 网关的消息约定。
//
// 上行：文本帧是控制指令，按 action 字段路由（交给 wsx.Router）；
//       二进制帧是音频数据（PCM16LE / 16kHz / 单声道 / 建议 100ms 一帧 = 3200 字节）。
// 下行：统一走 {"event":"<name>","data":{...}} 信封。

// ActionKey 是上行控制指令的路由字段名，对应 wsx.Router.EventKey。
const ActionKey = "action"

// 上行 action
const (
	ActionStart = "start"
	ActionStop  = "stop"
)

// 下行 event
const (
	EventReady   = "ready"
	EventPartial = "partial"
	EventFinal   = "final"
	EventDone    = "done"
	EventError   = "error"
)

// envelope 下行统一信封。
//
// 原来由 ehttp 的 Emit 负责，换到 wsx 之后信封形状由本包自己定义——
// 前端协议不能因为底层换了个封装就变形。
type envelope struct {
	Event string `json:"event"`
	Data  any    `json:"data,omitempty"`
}

// StartRequest 开始一次转录。字段留空时用服务端配置的默认值。
type StartRequest struct {
	Action              string   `json:"action"`
	Format              string   `json:"format"`
	SampleRate          int      `json:"sample_rate"`
	LanguageHints       []string `json:"language_hints"`
	SemanticPunctuation *bool    `json:"semantic_punctuation"`
	MaxSentenceSilence  int      `json:"max_sentence_silence"`
}

type ReadyData struct {
	SessionID string `json:"session_id"`
}

// SentenceData partial 与 final 共用。
type SentenceData struct {
	SentenceID int    `json:"sentence_id"`
	Text       string `json:"text"`
	BeginMs    int64  `json:"begin_ms"`
	EndMs      int64  `json:"end_ms"`
}

type DoneData struct {
	DurationSec int `json:"duration_sec"`
}

type ErrorData struct {
	Code         int    `json:"code"`
	Msg          string `json:"msg"`
	UpstreamCode string `json:"upstream_code,omitempty"`
}
