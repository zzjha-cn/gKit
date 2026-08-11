package logger

import (
	"context"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Logger 定义了业务层使用的日志接口，屏蔽底层 zap 实现
type Logger interface {
	Debug(args ...interface{})
	Info(args ...interface{})
	Warn(args ...interface{})
	Error(args ...interface{})
	Fatal(args ...interface{})

	Debugf(template string, args ...interface{})
	Infof(template string, args ...interface{})
	Warnf(template string, args ...interface{})
	Errorf(template string, args ...interface{})
	Fatalf(template string, args ...interface{})

	// 结构化键值对打印 (类似 zap.S().Infow)
	Debugw(msg string, keysAndValues ...interface{})
	Infow(msg string, keysAndValues ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
	Errorw(msg string, keysAndValues ...interface{})
	Fatalw(msg string, keysAndValues ...interface{})

	// With 附加结构化字段 (Key-Value)
	With(args ...interface{}) Logger
	// Ctx 从 context 中提取 TraceID 等上下文信息并附加到日志中
	Ctx(ctx context.Context) Logger
	// GetUnderlying 返回底层的日志实例 (通常是 *zap.Logger)
	GetUnderlying() any
}

var (
	// Log 暴露底层的 zap.Logger，仅供极少数需要极致性能的特殊场景使用
	Log *zap.Logger
	// global 内部全局接口实例
	global Logger
)

// zapLogger 是 Logger 接口的 zap 实现
type zapLogger struct {
	sugar *zap.SugaredLogger
}

func (l *zapLogger) Debug(args ...interface{}) { l.sugar.Debug(args...) }
func (l *zapLogger) Info(args ...interface{})  { l.sugar.Info(args...) }
func (l *zapLogger) Warn(args ...interface{})  { l.sugar.Warn(args...) }
func (l *zapLogger) Error(args ...interface{}) { l.sugar.Error(args...) }
func (l *zapLogger) Fatal(args ...interface{}) { l.sugar.Fatal(args...) }

func (l *zapLogger) Debugf(template string, args ...interface{}) { l.sugar.Debugf(template, args...) }
func (l *zapLogger) Infof(template string, args ...interface{})  { l.sugar.Infof(template, args...) }
func (l *zapLogger) Warnf(template string, args ...interface{})  { l.sugar.Warnf(template, args...) }
func (l *zapLogger) Errorf(template string, args ...interface{}) { l.sugar.Errorf(template, args...) }
func (l *zapLogger) Fatalf(template string, args ...interface{}) { l.sugar.Fatalf(template, args...) }

func (l *zapLogger) Debugw(msg string, keysAndValues ...interface{}) {
	if v, ok := findLogField(keysAndValues); ok {
		keysAndValues = v
	}
	l.sugar.Debugw(msg, keysAndValues...)
}
func (l *zapLogger) Infow(msg string, keysAndValues ...interface{}) {
	if v, ok := findLogField(keysAndValues); ok {
		keysAndValues = v
	}
	l.sugar.Infow(msg, keysAndValues...)
}
func (l *zapLogger) Warnw(msg string, keysAndValues ...interface{}) {
	if v, ok := findLogField(keysAndValues); ok {
		keysAndValues = v
	}
	l.sugar.Warnw(msg, keysAndValues...)
}
func (l *zapLogger) Errorw(msg string, keysAndValues ...interface{}) {
	if v, ok := findLogField(keysAndValues); ok {
		keysAndValues = v
	}
	l.sugar.Errorw(msg, keysAndValues...)
}
func (l *zapLogger) Fatalw(msg string, keysAndValues ...interface{}) {
	if v, ok := findLogField(keysAndValues); ok {
		keysAndValues = v
	}
	l.sugar.Fatalw(msg, keysAndValues...)
}

func (l *zapLogger) With(args ...interface{}) Logger {
	return &zapLogger{sugar: l.sugar.With(args...)}
}

func (l *zapLogger) GetUnderlying() any {
	return l.sugar.Desugar()
}

func (l *zapLogger) Ctx(ctx context.Context) Logger {
	if ctx == nil {
		return l
	}
	// 行业最佳实践：从 context 中提取链路追踪 ID (TraceID) 或 UserID
	// 这里预留扩展点，假设 context 中的 key 为 "trace_id"
	if traceID, ok := ctx.Value("trace_id").(string); ok {
		return l.With("trace_id", traceID)
	}
	if userID, ok := ctx.Value("user_id").(string); ok {
		return l.With("user_id", userID)
	}
	return l
}

// NewLogger 创建一个新的独立 Logger 实例，可用于将特定业务日志输出到不同文件
// logPath: 常规日志路径 (如 ./logs/app.log)
// errLogPath: 错误日志路径 (如 ./logs/error.log)，如果为空则不单独分离错误日志
func NewLogger(mode string, logPath string, errLogPath string) (Logger, error) {
	var core zapcore.Core

	if mode == "debug" {
		encoderConfig := zap.NewDevelopmentEncoderConfig()
		encoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		encoder := zapcore.NewConsoleEncoder(encoderConfig)
		core = zapcore.NewCore(encoder, zapcore.AddSync(os.Stdout), zap.DebugLevel)
	} else {
		encoderConfig := zap.NewProductionEncoderConfig()
		encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		encoder := zapcore.NewJSONEncoder(encoderConfig)

		// 1. 常规日志 (Info 级别及以上)
		infoHook := &lumberjack.Logger{
			Filename:   logPath,
			MaxSize:    100,
			MaxBackups: 30,
			MaxAge:     7,
			Compress:   true,
		}
		infoCore := zapcore.NewCore(encoder, zapcore.AddSync(infoHook), zap.InfoLevel)

		// 2. 错误日志 (Error 级别及以上)
		var cores []zapcore.Core
		cores = append(cores, infoCore)

		var errHook *lumberjack.Logger
		if errLogPath != "" {
			errHook = &lumberjack.Logger{
				Filename:   errLogPath,
				MaxSize:    100,
				MaxBackups: 30,
				MaxAge:     7,
				Compress:   true,
			}
			errCore := zapcore.NewCore(encoder, zapcore.AddSync(errHook), zap.ErrorLevel)
			cores = append(cores, errCore)
		}

		// 3. 启动后台协程，每天零点强制触发日志切割 (按天切割)
		go func() {
			for {
				now := time.Now()
				// 计算距离明天零点还有多久
				next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
				time.Sleep(time.Until(next))

				// 零点时刻，强制轮转
				_ = infoHook.Rotate()
				if errHook != nil {
					_ = errHook.Rotate()
				}
			}
		}()

		// 使用 NewTee 将日志分发到多个 Core
		core = zapcore.NewTee(cores...)
	}

	// 注意：AddCallerSkip(1) 非常关键！因为我们封装了一层 Logger 接口，
	// 如果不跳过一层调用栈，日志里打印的文件名和行号永远是 logger.go
	zLog := zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))

	return &zapLogger{sugar: zLog.Sugar()}, nil
}

// InitLogger 初始化全局日志记录器
func InitLogger(mode string, logPath string, errLogPath string) error {
	l, err := NewLogger(mode, logPath, errLogPath)
	if err != nil {
		return err
	}

	global = l
	Log = Unwrap[*zap.Logger](l)
	zap.ReplaceGlobals(Log)

	return nil
}

// Sync 刷新所有缓冲的日志条目
func Sync() {
	if Log != nil {
		_ = Log.Sync()
	}
}

// -------------------------------------------------------------------
// 导出的包级别函数，方便业务层直接调用 logger.Info() 等
// -------------------------------------------------------------------

func Debug(args ...interface{}) { global.Debug(args...) }
func Info(args ...interface{})  { global.Info(args...) }
func Warn(args ...interface{})  { global.Warn(args...) }
func Error(args ...interface{}) { global.Error(args...) }
func Fatal(args ...interface{}) { global.Fatal(args...) }

func Debugf(template string, args ...interface{}) { global.Debugf(template, args...) }
func Infof(template string, args ...interface{})  { global.Infof(template, args...) }
func Warnf(template string, args ...interface{})  { global.Warnf(template, args...) }
func Errorf(template string, args ...interface{}) { global.Errorf(template, args...) }
func Fatalf(template string, args ...interface{}) { global.Fatalf(template, args...) }

func Debugw(msg string, keysAndValues ...interface{}) { global.Debugw(msg, keysAndValues...) }
func Infow(msg string, keysAndValues ...interface{})  { global.Infow(msg, keysAndValues...) }
func Warnw(msg string, keysAndValues ...interface{})  { global.Warnw(msg, keysAndValues...) }
func Errorw(msg string, keysAndValues ...interface{}) { global.Errorw(msg, keysAndValues...) }
func Fatalw(msg string, keysAndValues ...interface{}) { global.Fatalw(msg, keysAndValues...) }

func With(args ...interface{}) Logger { return global.With(args...) }
func Ctx(ctx context.Context) Logger  { return global.Ctx(ctx) }

// -------------------------------------------------------------------
// 泛型支持：获取底层日志实例 (用于极致性能场景)
// -------------------------------------------------------------------

// Unwrap 提取 Logger 接口底层的具体实现，支持泛型。
// 示例:
//
//	zapL := logger.Unwrap[*zap.Logger](logger.Ctx(ctx))
//	zapS := logger.Unwrap[*zap.SugaredLogger](logger.Ctx(ctx))
func Unwrap[T any](l Logger) T {
	var zero T
	if l == nil {
		return zero
	}
	underlying := l.GetUnderlying()

	var target any = zero
	switch target.(type) {
	case *zap.Logger:
		if z, ok := underlying.(*zap.Logger); ok {
			return any(z).(T)
		}
	case *zap.SugaredLogger:
		if z, ok := underlying.(*zap.Logger); ok {
			return any(z.Sugar()).(T)
		}
	}
	return zero
}

// Get 获取全局的底层日志实例，支持泛型。
// 示例:
//
//	zapL := logger.Get[*zap.Logger]()
//	zapS := logger.Get[*zap.SugaredLogger]()
func Get[T any]() T {
	return Unwrap[T](global)
}
