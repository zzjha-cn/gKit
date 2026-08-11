package logger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestNewLogger(t *testing.T) {
	tempDir := "./"
	logFile := filepath.Join(tempDir, "audit.log")
	errLogFile := filepath.Join(tempDir, "error.log")

	// 创建一个独立的 logger，输出到 audit.log，错误输出到 error.log
	auditLogger, err := NewLogger("release", logFile, errLogFile)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	auditLogger.Info("this is an audit message")
	// auditLogger.Infow("user action", "action", "login", "user_id", 123)
	auditLogger.Infow("user action", LogField{"action": "login", "user_id": 123, "data": LogField{"req": "req_id", "session": 1231213}})
	auditLogger.Error("this is an error message")

	// 刷新缓冲区以确保写入文件
	if zLog := Unwrap[*zap.Logger](auditLogger); zLog != nil {
		_ = zLog.Sync()
	}

	// 读取常规文件内容验证
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	strContent := string(content)
	if !strings.Contains(strContent, "this is an audit message") {
		t.Errorf("Log file does not contain expected message. Content: %s", strContent)
	}
	if !strings.Contains(strContent, `"action":"login"`) {
		t.Errorf("Log file does not contain structured data. Content: %s", strContent)
	}
	if !strings.Contains(strContent, "this is an error message") {
		t.Errorf("Log file should also contain error messages. Content: %s", strContent)
	}

	// 读取错误文件内容验证
	errContent, err := os.ReadFile(errLogFile)
	if err != nil {
		t.Fatalf("Failed to read error log file: %v", err)
	}
	strErrContent := string(errContent)
	if !strings.Contains(strErrContent, "this is an error message") {
		t.Errorf("Error log file does not contain expected error message. Content: %s", strErrContent)
	}
	if strings.Contains(strErrContent, "this is an audit message") {
		t.Errorf("Error log file should NOT contain info messages. Content: %s", strErrContent)
	}
}

func TestCtxAndWith(t *testing.T) {
	tempDir := "./"
	logFile := filepath.Join(tempDir, "ctx.log")

	l, _ := NewLogger("release", logFile, "")

	// 模拟带有 trace_id 的 context
	ctx := context.WithValue(context.Background(), "trace_id", "trace-12345")

	// 测试 Ctx 和 With 链式调用
	l.Ctx(ctx).With("extra_key", "extra_value").Info("context message")

	if zLog := Unwrap[*zap.Logger](l); zLog != nil {
		_ = zLog.Sync()
	}

	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	strContent := string(content)
	if !strings.Contains(strContent, `"trace_id":"trace-12345"`) {
		t.Errorf("Log does not contain trace_id. Content: %s", strContent)
	}
	if !strings.Contains(strContent, `"extra_key":"extra_value"`) {
		t.Errorf("Log does not contain extra_key. Content: %s", strContent)
	}
}

func TestUnwrap(t *testing.T) {
	l, _ := NewLogger("debug", "", "")

	// 测试解包为 *zap.Logger
	zapL := Unwrap[*zap.Logger](l)
	if zapL == nil {
		t.Error("Failed to unwrap *zap.Logger")
	}

	// 测试解包为 *zap.SugaredLogger
	zapS := Unwrap[*zap.SugaredLogger](l)
	if zapS == nil {
		t.Error("Failed to unwrap *zap.SugaredLogger")
	}
}

func TestGlobalLogger(t *testing.T) {
	tempDir := "./"
	logFile := filepath.Join(tempDir, "global.log")

	err := InitLogger("release", logFile, "")
	if err != nil {
		t.Fatalf("Failed to init global logger: %v", err)
	}

	Info("global info message")
	Sync()

	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	if !strings.Contains(string(content), "global info message") {
		t.Errorf("Global log file does not contain expected message")
	}
}
