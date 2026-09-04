// Package observability 横切关注点：zap 日志 + 产出物契约 RunCtx（D1 核心机制）。
package observability

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// defaultLogger 进程级单例日志器。
var defaultLogger *zap.Logger

func init() {
	defaultLogger = NewLogger()
}

// NewLogger 构建结构化日志器（控制台输出，级别 INFO）。
// 注：文件轮转 + 30 天保留在后续批次接入（Batch 1 仅控制台，满足结构化与掩码需求）。
func NewLogger() *zap.Logger {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	encCfg.TimeKey = "ts"
	enc := zapcore.NewConsoleEncoder(encCfg)

	core := zapcore.NewCore(enc, zapcore.Lock(os.Stdout), zapcore.InfoLevel)
	return zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
}

// L 返回进程级日志器（未初始化时返回 Nop，避免 nil panic）。
func L() *zap.Logger {
	if defaultLogger == nil {
		return zap.NewNop()
	}
	return defaultLogger
}

// S 返回进程级糖化日志器（键值对风格：S().Infow("msg", "k", v)）。
func S() *zap.SugaredLogger {
	return L().Sugar()
}

// Sync 刷新缓冲（进程退出前调用）。
func Sync() {
	if defaultLogger != nil {
		_ = defaultLogger.Sync()
	}
}

// Mask 对凭据字段做掩码（统一脱敏入口，供日志/告警调用）。
func Mask(value string) string {
	if value == "" {
		return ""
	}
	return "****"
}
