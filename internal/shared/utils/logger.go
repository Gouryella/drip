package utils

import (
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var logger *zap.Logger

// InitLogger initializes the global logger for client
// verbose: if true, shows debug level logs; if false, shows error level only
func InitLogger(verbose bool) error {
	var config zap.Config

	if verbose {
		// Verbose mode: show debug and above
		config = zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		// Production mode: only show errors
		config = zap.NewProductionConfig()
		config.Level = zap.NewAtomicLevelAt(zapcore.ErrorLevel)
	}

	config.OutputPaths = []string{"stdout"}
	config.ErrorOutputPaths = []string{"stderr"}

	var err error
	logger, err = config.Build()
	if err != nil {
		return err
	}

	return nil
}

// InitServerLogger initializes logger for server with info level by default
func InitServerLogger(debug bool) error {
	var config zap.Config

	if debug {
		// Debug mode: show all logs
		config = zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		// Production mode: show info and above
		config = zap.NewProductionConfig()
	}

	config.OutputPaths = []string{"stdout"}
	config.ErrorOutputPaths = []string{"stderr"}

	var err error
	logger, err = config.Build()
	if err != nil {
		return err
	}

	return nil
}

// GetLogger returns the global logger instance
func GetLogger() *zap.Logger {
	if logger == nil {
		// Fallback to a basic logger if not initialized
		logger, _ = zap.NewProduction()
	}
	return logger
}

// Info logs an info message
func Info(msg string, fields ...zap.Field) {
	GetLogger().Info(msg, fields...)
}

// Debug logs a debug message
func Debug(msg string, fields ...zap.Field) {
	GetLogger().Debug(msg, fields...)
}

// Warn logs a warning message
func Warn(msg string, fields ...zap.Field) {
	GetLogger().Warn(msg, fields...)
}

// Error logs an error message
func Error(msg string, fields ...zap.Field) {
	GetLogger().Error(msg, fields...)
}

// Fatal logs a fatal message and exits
func Fatal(msg string, fields ...zap.Field) {
	GetLogger().Fatal(msg, fields...)
	os.Exit(1)
}

// Sync flushes any buffered log entries
func Sync() {
	if logger != nil {
		_ = logger.Sync()
	}
}

// maxLogFileBytes is the size at which a service log file is rotated.
const maxLogFileBytes = 10 << 20

// InitFileLogger initializes the global logger to append JSON lines to path
// instead of writing to stdout. The Windows service has no console attached, so
// anything written to stdout there is discarded.
//
// The core is assembled by hand rather than through zap.Config.OutputPaths
// because zap parses output paths as URLs, and a Windows path like
// C:\ProgramData\drip\logs\service.log is rejected as an unknown "c" scheme.
func InitFileLogger(path string, verbose bool) error {
	if path == "" {
		return fmt.Errorf("log path is required")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	if err := rotateLogFile(path, maxLogFileBytes); err != nil {
		return err
	}

	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- the log path is chosen by the administrator installing the service
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	level := zapcore.InfoLevel
	if verbose {
		level = zapcore.DebugLevel
	}

	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	sink := zapcore.Lock(zapcore.AddSync(file))
	logger = zap.New(
		zapcore.NewCore(zapcore.NewJSONEncoder(encoderConfig), sink, level),
		zap.ErrorOutput(sink),
	)

	return nil
}

// rotateLogFile renames path to path+".1" once it grows past maxBytes, keeping a
// single previous generation. A service can run for months; an unbounded log is
// a disk-full waiting to happen.
func rotateLogFile(path string, maxBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to stat log file: %w", err)
	}

	if info.Size() < maxBytes {
		return nil
	}

	previous := path + ".1"
	if err := os.Remove(previous); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove rotated log file: %w", err)
	}
	if err := os.Rename(path, previous); err != nil {
		return fmt.Errorf("failed to rotate log file: %w", err)
	}

	return nil
}
