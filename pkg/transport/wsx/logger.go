package wsx

// Logger 是 wsx 需要的最小日志接口。
//
// 只声明结构化打印方法，pkg/logger.Logger 天然满足该接口，
// 因此业务可以直接注入项目内的 logger，而 wsx 不依赖任何具体实现。
type Logger interface {
	Debugw(msg string, keysAndValues ...interface{})
	Infow(msg string, keysAndValues ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
	Errorw(msg string, keysAndValues ...interface{})
}

// nopLogger 是未注入 Logger 时的默认实现，所有输出丢弃。
type nopLogger struct{}

func (nopLogger) Debugw(string, ...interface{}) {}
func (nopLogger) Infow(string, ...interface{})  {}
func (nopLogger) Warnw(string, ...interface{})  {}
func (nopLogger) Errorw(string, ...interface{}) {}
