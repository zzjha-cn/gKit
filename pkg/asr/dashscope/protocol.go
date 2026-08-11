package dashscope

import "strings"

// 协议字段对照百炼官方「客户端事件」「服务端事件」文档：(2026-08-07)
// https://help.aliyun.com/zh/model-studio/fun-asr-client-events
// https://help.aliyun.com/zh/model-studio/fun-asr-server-events

// 客户端指令 action
const (
	ActionRunTask    = "run-task"
	ActionFinishTask = "finish-task"

	StreamingDuplex = "duplex"

	TaskGroupAudio      = "audio"
	TaskASR             = "asr"
	FunctionRecognition = "recognition"
)

// 服务端事件 event
const (
	EventTaskStarted     = "task-started"
	EventResultGenerated = "result-generated"
	EventTaskFinished    = "task-finished"
	EventTaskFailed      = "task-failed"
)

// retryableUpstreamCodes 明确判定为上游瞬时故障、重连有意义的错误码。
//
// 默认不重试：未知错误码宁可直接失败，也不要在上游已经异常时继续加压。
// 鉴权（InvalidApiKey / AccessDenied）、参数（InvalidParameter）、
// 配额（Throttling / Arrearage）这类确定性错误重试多少次都是同样结果。
// 线上观察到新的可重试码往这里加。
var retryableUpstreamCodes = map[string]struct{}{
	"InternalError":      {},
	"SystemError":        {},
	"ServiceUnavailable": {},
	"RequestTimeOut":     {},
	"ModelUnavailable":   {},
}

// isRetryableUpstreamCode 判断 task-failed 的 error_code 是否值得重连。
func isRetryableUpstreamCode(code string) bool {
	if code == "" {
		return false
	}
	if _, ok := retryableUpstreamCodes[code]; ok {
		return true
	}
	// 阿里云错误码常见 "InternalError.Algo" 这种带子类的形式，按主类判定。
	if i := strings.Index(code, "."); i > 0 {
		_, ok := retryableUpstreamCodes[code[:i]]
		return ok
	}
	return false
}

type clientHeader struct {
	Action    string `json:"action"`
	TaskID    string `json:"task_id"`
	Streaming string `json:"streaming"`
}

// runTaskRequest 开启识别任务。
type runTaskRequest struct {
	Header  clientHeader   `json:"header"`
	Payload runTaskPayload `json:"payload"`
}

type runTaskPayload struct {
	TaskGroup  string        `json:"task_group"`
	Task       string        `json:"task"`
	Function   string        `json:"function"`
	Model      string        `json:"model"`
	Parameters taskParameter `json:"parameters"`
	Input      struct{}      `json:"input"`
}

// taskParameter 只声明本项目会用到的参数，其余可选项（vocabulary_id、
// speech_noise_threshold、special_word_filter 等）按需再加。
type taskParameter struct {
	Format                    string   `json:"format"`
	SampleRate                int      `json:"sample_rate"`
	LanguageHints             []string `json:"language_hints,omitempty"`
	SemanticPunctuationEnable bool     `json:"semantic_punctuation_enabled,omitempty"`
	MaxSentenceSilence        int      `json:"max_sentence_silence,omitempty"`
	VocabularyID              string   `json:"vocabulary_id,omitempty"`
	Heartbeat                 bool     `json:"heartbeat,omitempty"`
}

// finishTaskRequest 通知服务端音频发送完毕。
type finishTaskRequest struct {
	Header  clientHeader      `json:"header"`
	Payload finishTaskPayload `json:"payload"`
}

type finishTaskPayload struct {
	Input struct{} `json:"input"`
}

// serverEvent 服务端下行事件，按 header.event 分发。
type serverEvent struct {
	Header  serverHeader  `json:"header"`
	Payload serverPayload `json:"payload"`
}

type serverHeader struct {
	TaskID       string `json:"task_id"`
	Event        string `json:"event"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

type serverPayload struct {
	Output struct {
		Sentence *sentence `json:"sentence"`
	} `json:"output"`
	// Usage 在 sentence_end=false 时为 null。
	Usage *usage `json:"usage"`
}

type sentence struct {
	BeginTime   int64  `json:"begin_time"`
	EndTime     int64  `json:"end_time"`
	Text        string `json:"text"`
	Heartbeat   bool   `json:"heartbeat"`
	SentenceEnd bool   `json:"sentence_end"`
	SentenceID  int    `json:"sentence_id"`
	Words       []word `json:"words"`
}

type word struct {
	BeginTime   int64  `json:"begin_time"`
	EndTime     int64  `json:"end_time"`
	Text        string `json:"text"`
	Punctuation string `json:"punctuation"`
}

type usage struct {
	Duration int `json:"duration"` // 计费秒数
}
