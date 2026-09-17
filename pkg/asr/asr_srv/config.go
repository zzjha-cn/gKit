package asrsrv

// ASRCfg 实时语音识别（百炼 WebSocket）配置。
type ASRCfg struct {
	Region      string `yaml:"region"`       // cn-beijing | ap-southeast-1
	WorkspaceID string `yaml:"workspace_id"` // 拼进 wss 地址的子域
	APIKey      string `yaml:"api_key"`
	Model       string `yaml:"model"`
	Endpoint    string `yaml:"endpoint"` // 覆盖默认 wss 地址，留空按 region+workspace 拼

	Token         string   `yaml:"token"`          // 前端握手 token，为空表示不校验
	AllowOrigins  []string `yaml:"allow_origins"`  // WS 来源白名单，为空表示不限制
	MaxConcurrent int      `yaml:"max_concurrent"` // 并发会话上限

	SampleRate          int  `yaml:"sample_rate"`           // 默认 16000
	MaxSentenceSilence  int  `yaml:"max_sentence_silence"`  // VAD 断句阈值，200-6000ms
	SemanticPunctuation bool `yaml:"semantic_punctuation"`  // 语义断句标点
	AudioBufferFrames   int  `yaml:"audio_buffer_frames"`   // 音频有界队列长度，默认 50（≈5s）
	UpstreamDialTimeout int  `yaml:"upstream_dial_timeout"` // 单次拨号+等待 task-started 秒数，默认 4

	UpstreamStartAttempts int `yaml:"upstream_start_attempts"` // 建立上游会话的尝试次数（含首次），默认 3
	UpstreamStartTimeout  int `yaml:"upstream_start_timeout"`  // 建立会话的总预算秒数（覆盖全部重试），默认 10
	UpstreamReadTimeout   int `yaml:"upstream_read_timeout"`   // 上游静默多久判定链路已死，默认 60

	StartWaitSeconds  int `yaml:"start_wait_seconds"`  // 连上后等 start 指令的秒数，默认 30
	DrainWaitSeconds  int `yaml:"drain_wait_seconds"`  // 发出 finish-task 后等上游收尾的秒数，默认 10
	MaxSessionSeconds int `yaml:"max_session_seconds"` // 单次会话最长时长，默认 600
}

// ASR 默认值，避免配置缺项时行为不可预期。
const (
	DefaultASRRegion             = "cn-beijing"
	DefaultASRModel              = "qwen-audio-3.0-asr-flash-streaming"
	DefaultASRSampleRate         = 16000
	DefaultASRMaxSentenceSilence = 1300
	DefaultASRMaxConcurrent      = 20
	DefaultASRAudioBufferFrames  = 50
	// DefaultASRDialTimeout 单次尝试的超时取值偏小，是为了给重试留出预算：
	// 3 次 × 4s + 退避 ≈ 13s，再由 DefaultASRStartTimeout 兜底截断。
	DefaultASRDialTimeout   = 4
	DefaultASRStartAttempts = 3
	DefaultASRStartTimeout  = 10
	DefaultASRReadTimeout   = 60
	DefaultASRStartWait     = 30
	DefaultASRDrainWait     = 10
	DefaultASRMaxSession    = 600
)

// Normalize 补齐零值，返回自身便于链式使用。
func (c *ASRCfg) Normalize() *ASRCfg {
	if c == nil {
		return nil
	}
	if c.Region == "" {
		c.Region = DefaultASRRegion
	}
	if c.Model == "" {
		c.Model = DefaultASRModel
	}
	if c.SampleRate <= 0 {
		c.SampleRate = DefaultASRSampleRate
	}
	if c.MaxSentenceSilence <= 0 {
		c.MaxSentenceSilence = DefaultASRMaxSentenceSilence
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = DefaultASRMaxConcurrent
	}
	if c.AudioBufferFrames <= 0 {
		c.AudioBufferFrames = DefaultASRAudioBufferFrames
	}
	if c.UpstreamDialTimeout <= 0 {
		c.UpstreamDialTimeout = DefaultASRDialTimeout
	}
	if c.UpstreamStartAttempts <= 0 {
		c.UpstreamStartAttempts = DefaultASRStartAttempts
	}
	if c.UpstreamStartTimeout <= 0 {
		c.UpstreamStartTimeout = DefaultASRStartTimeout
	}
	if c.UpstreamReadTimeout <= 0 {
		c.UpstreamReadTimeout = DefaultASRReadTimeout
	}
	if c.StartWaitSeconds <= 0 {
		c.StartWaitSeconds = DefaultASRStartWait
	}
	if c.DrainWaitSeconds <= 0 {
		c.DrainWaitSeconds = DefaultASRDrainWait
	}
	if c.MaxSessionSeconds <= 0 {
		c.MaxSessionSeconds = DefaultASRMaxSession
	}
	return c
}
