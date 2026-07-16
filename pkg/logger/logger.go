package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var globalLogger *zap.SugaredLogger

// DailyWriteSyncer 按日期切割的日志写入器
// 每天一个日志文件: app-2026-07-15.log
// 自动清理超过保留天数的旧日志
type DailyWriteSyncer struct {
	dir         string
	prefix      string
	retainDays  int
	currentFile *os.File
	currentDate string
}

// NewDailyWriteSyncer 创建按天切割的日志写入器
// dir: 日志目录
// prefix: 文件名前缀 (如 "app")
// retainDays: 保留天数 (如 7)
func NewDailyWriteSyncer(dir, prefix string, retainDays int) (*DailyWriteSyncer, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	d := &DailyWriteSyncer{
		dir:        dir,
		prefix:     prefix,
		retainDays: retainDays,
	}
	// 启动时清理旧日志
	d.cleanup()
	// 打开今天的文件
	if err := d.rotateIfNeeded(); err != nil {
		return nil, err
	}
	return d, nil
}

// Write 实现 zapcore.WriteSyncer 接口
func (d *DailyWriteSyncer) Write(p []byte) (n int, err error) {
	if err := d.rotateIfNeeded(); err != nil {
		return 0, err
	}
	return d.currentFile.Write(p)
}

// Sync 实现 zapcore.WriteSyncer 接口
func (d *DailyWriteSyncer) Sync() error {
	if d.currentFile != nil {
		return d.currentFile.Sync()
	}
	return nil
}

// rotateIfNeeded 检查是否需要切换到新的日期文件
func (d *DailyWriteSyncer) rotateIfNeeded() error {
	today := time.Now().Format("2006-01-02")
	if today == d.currentDate && d.currentFile != nil {
		return nil
	}
	// 关闭旧文件
	if d.currentFile != nil {
		d.currentFile.Close()
	}
	// 打开新文件
	filename := filepath.Join(d.dir, fmt.Sprintf("%s-%s.log", d.prefix, today))
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	d.currentFile = f
	d.currentDate = today
	// 每天第一次写日志时清理旧文件
	go d.cleanup()
	return nil
}

// cleanup 清理超过保留天数的日志文件
func (d *DailyWriteSyncer) cleanup() {
	if d.retainDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -d.retainDays)
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// 只处理匹配前缀和 .log 后缀的文件
		if !strings.HasPrefix(name, d.prefix+"-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		// 从文件名提取日期: prefix-YYYY-MM-DD.log
		dateStr := name[len(d.prefix)+1 : len(name)-4]
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			os.Remove(filepath.Join(d.dir, name))
		}
	}
}

// Init 初始化日志
func Init(level, format, output, filePath string) error {
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "info":
		zapLevel = zapcore.InfoLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05"),
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	var encoder zapcore.Encoder
	if format == "json" {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	var writeSyncer zapcore.WriteSyncer
	if output == "file" {
		dir := filepath.Dir(filePath)
		base := filepath.Base(filePath)
		// 从 base 提取前缀 (去掉 .log 后缀)
		prefix := strings.TrimSuffix(base, filepath.Ext(base))
		// 默认保留 7 天
		retainDays := 7
		daily, err := NewDailyWriteSyncer(dir, prefix, retainDays)
		if err != nil {
			return err
		}
		writeSyncer = daily
	} else {
		writeSyncer = zapcore.AddSync(os.Stdout)
	}

	core := zapcore.NewCore(encoder, writeSyncer, zapLevel)
	zapLogger := zap.New(core, zap.AddCaller(), zap.AddCallerSkip(0))
	globalLogger = zapLogger.Sugar()
	return nil
}

// L 获取全局logger
func L() *zap.SugaredLogger {
	if globalLogger == nil {
		// 默认logger
		zapLogger, _ := zap.NewProduction()
		globalLogger = zapLogger.Sugar()
	}
	return globalLogger
}

// Sync 刷新日志
func Sync() {
	if globalLogger != nil {
		globalLogger.Sync()
	}
}
