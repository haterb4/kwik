package logger

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/s-anzie/kwik/internal/protocol"
)

// LogLevel represents the logging level
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
	LogLevelSilent
)

// Logger interface for KWIK system logging
type Logger interface {
	Debug(msg string, keysAndValues ...interface{})
	Info(msg string, keysAndValues ...interface{})
	Warn(msg string, keysAndValues ...interface{})
	Error(msg string, keysAndValues ...interface{})
	SetLevel(level LogLevel)

	// Enhanced logging methods
	DebugWithContext(ctx context.Context, msg string, keysAndValues ...interface{})
	InfoWithContext(ctx context.Context, msg string, keysAndValues ...interface{})
	WarnWithContext(ctx context.Context, msg string, keysAndValues ...interface{})
	ErrorWithContext(ctx context.Context, msg string, keysAndValues ...interface{})

	// Error-specific logging
	LogError(err error, msg string, keysAndValues ...interface{})
	LogKwikError(err *protocol.KwikError, msg string, keysAndValues ...interface{})

	// Component-specific logging
	WithComponent(component string) Logger
	WithSession(sessionID string) Logger
	WithPath(pathID string) Logger
	WithStream(streamID uint64) Logger

	// Performance logging
	LogPerformance(operation string, duration time.Duration, keysAndValues ...interface{})
	LogMetrics(metrics map[string]interface{})
}

// loggerImpl is a basic implementation of the Logger interface
type loggerImpl struct {
	level     LogLevel
	logger    *log.Logger
	component string
	sessionID string
	pathID    string
	streamID  uint64
	mutex     sync.RWMutex
}

// NewLogger creates a new logger with the specified level
func NewLogger(level LogLevel) Logger {
	return &loggerImpl{
		level:  level,
		logger: log.New(os.Stdout, "[KWIK] ", log.LstdFlags|log.Lmicroseconds),
	}
}

// Debug logs a debug message
func (l *loggerImpl) Debug(msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelDebug && l.level != LogLevelSilent {
		l.logWithLevel("DEBUG", msg, keysAndValues...)
	}
}

// Info logs an info message
func (l *loggerImpl) Info(msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelInfo && l.level != LogLevelSilent {
		l.logWithLevel("INFO", msg, keysAndValues...)
	}
}

// Warn logs a warning message
func (l *loggerImpl) Warn(msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelWarn && l.level != LogLevelSilent {
		l.logWithLevel("WARN", msg, keysAndValues...)
	}
}

// Error logs an error message
func (l *loggerImpl) Error(msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelError && l.level != LogLevelSilent {
		l.logWithLevel("ERROR", msg, keysAndValues...)
	}
}

// SetLevel sets the logging level
func (l *loggerImpl) SetLevel(level LogLevel) {
	l.level = level
}

func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "DEBUG"
	case LogLevelInfo:
		return "INFO"
	case LogLevelWarn:
		return "WARN"
	case LogLevelError:
		return "ERROR"
	case LogLevelSilent:
		return "SILENT"
	default:
		return "UNKNOWN"
	}
}

// Enhanced logging methods with context
func (l *loggerImpl) DebugWithContext(ctx context.Context, msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelDebug && l.level != LogLevelSilent {
		l.logWithContext(ctx, "DEBUG", msg, keysAndValues...)
	}
}

func (l *loggerImpl) InfoWithContext(ctx context.Context, msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelInfo && l.level != LogLevelSilent {
		l.logWithContext(ctx, "INFO", msg, keysAndValues...)
	}
}

func (l *loggerImpl) WarnWithContext(ctx context.Context, msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelWarn && l.level != LogLevelSilent {
		l.logWithContext(ctx, "WARN", msg, keysAndValues...)
	}
}

func (l *loggerImpl) ErrorWithContext(ctx context.Context, msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelError && l.level != LogLevelSilent {
		l.logWithContext(ctx, "ERROR", msg, keysAndValues...)
	}
}

// Error-specific logging methods
func (l *loggerImpl) LogError(err error, msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelError && l.level != LogLevelSilent {
		kvs := append(keysAndValues, "error", err.Error())
		l.logWithLevel("ERROR", msg, kvs...)
	}
}

func (l *loggerImpl) LogKwikError(err *protocol.KwikError, msg string, keysAndValues ...interface{}) {
	if l.level <= LogLevelError && l.level != LogLevelSilent {
		kvs := append(keysAndValues,
			"errorCode", err.Code,
			"errorMessage", err.Message,
			"error", err.Error())
		if err.Cause != nil {
			kvs = append(kvs, "cause", err.Cause.Error())
		}
		l.logWithLevel("ERROR", msg, kvs...)
	}
}

// Component-specific logging methods
func (l *loggerImpl) WithComponent(component string) Logger {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	newLogger := &loggerImpl{
		level:     l.level,
		logger:    l.logger,
		component: component,
		sessionID: l.sessionID,
		pathID:    l.pathID,
		streamID:  l.streamID,
	}
	return newLogger
}

func (l *loggerImpl) WithSession(sessionID string) Logger {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	newLogger := &loggerImpl{
		level:     l.level,
		logger:    l.logger,
		component: l.component,
		sessionID: sessionID,
		pathID:    l.pathID,
		streamID:  l.streamID,
	}
	return newLogger
}

func (l *loggerImpl) WithPath(pathID string) Logger {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	newLogger := &loggerImpl{
		level:     l.level,
		logger:    l.logger,
		component: l.component,
		sessionID: l.sessionID,
		pathID:    pathID,
		streamID:  l.streamID,
	}
	return newLogger
}

func (l *loggerImpl) WithStream(streamID uint64) Logger {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	newLogger := &loggerImpl{
		level:     l.level,
		logger:    l.logger,
		component: l.component,
		sessionID: l.sessionID,
		pathID:    l.pathID,
		streamID:  streamID,
	}
	return newLogger
}

// Performance logging
func (l *loggerImpl) LogPerformance(operation string, duration time.Duration, keysAndValues ...interface{}) {
	if l.level <= LogLevelInfo && l.level != LogLevelSilent {
		kvs := append(keysAndValues,
			"operation", operation,
			"duration_ms", duration.Milliseconds(),
			"duration_us", duration.Microseconds())
		l.logWithLevel("PERF", "Performance measurement", kvs...)
	}
}

func (l *loggerImpl) LogMetrics(metrics map[string]interface{}) {
	if l.level <= LogLevelInfo && l.level != LogLevelSilent {
		var kvs []interface{}
		for key, value := range metrics {
			kvs = append(kvs, key, value)
		}
		l.logWithLevel("METRICS", "System metrics", kvs...)
	}
}

// logWithContext logs a message with context information
func (l *loggerImpl) logWithContext(ctx context.Context, level, msg string, keysAndValues ...interface{}) {
	// Extract context values if available
	var contextKVs []interface{}

	// Add trace information if available
	if traceID := ctx.Value("traceID"); traceID != nil {
		contextKVs = append(contextKVs, "traceID", traceID)
	}

	if requestID := ctx.Value("requestID"); requestID != nil {
		contextKVs = append(contextKVs, "requestID", requestID)
	}

	// Combine context and provided key-value pairs
	allKVs := append(contextKVs, keysAndValues...)

	l.logWithLevel(level, msg, allKVs...)
}

// Enhanced logWithLevel that includes component context
func (l *loggerImpl) logWithLevel(level, msg string, keysAndValues ...interface{}) {
	l.mutex.RLock()
	component := l.component
	sessionID := l.sessionID
	pathID := l.pathID
	streamID := l.streamID
	l.mutex.RUnlock()

	// Build context prefix
	var contextParts []string
	if component != "" {
		contextParts = append(contextParts, fmt.Sprintf("comp=%s", component))
	}
	if sessionID != "" {
		contextParts = append(contextParts, fmt.Sprintf("session=%s", sessionID))
	}
	if pathID != "" {
		contextParts = append(contextParts, fmt.Sprintf("path=%s", pathID))
	}
	if streamID != 0 {
		contextParts = append(contextParts, fmt.Sprintf("stream=%d", streamID))
	}

	var contextPrefix string
	if len(contextParts) > 0 {
		contextPrefix = fmt.Sprintf("[%s] ", strings.Join(contextParts, ","))
	}

	// Add caller information for debug level
	var callerInfo string
	if level == "DEBUG" || level == "ERROR" {
		if pc, file, line, ok := runtime.Caller(3); ok {
			funcName := runtime.FuncForPC(pc).Name()
			// Extract just the function name
			if idx := strings.LastIndex(funcName, "."); idx != -1 {
				funcName = funcName[idx+1:]
			}
			// Extract just the filename
			if idx := strings.LastIndex(file, "/"); idx != -1 {
				file = file[idx+1:]
			}
			callerInfo = fmt.Sprintf(" [%s:%d:%s]", file, line, funcName)
		}
	}

	// Format key-value pairs
	var kvPairs string
	if len(keysAndValues) > 0 {
		kvPairs = " "
		for i := 0; i < len(keysAndValues); i += 2 {
			if i+1 < len(keysAndValues) {
				kvPairs += fmt.Sprintf("%v=%v ", keysAndValues[i], keysAndValues[i+1])
			} else {
				kvPairs += fmt.Sprintf("%v=<missing_value> ", keysAndValues[i])
			}
		}
	}

	// Log the message with full context
	l.logger.Printf("[%s] %s%s%s%s", level, contextPrefix, msg, kvPairs, callerInfo)
}

// LoggingMiddleware provides logging middleware for operations
type LoggingMiddleware struct {
	logger Logger
}

// NewLoggingMiddleware creates a new logging middleware
func NewLoggingMiddleware(logger Logger) *LoggingMiddleware {
	return &LoggingMiddleware{logger: logger}
}

// WrapOperation wraps an operation with logging and error handling
func (lm *LoggingMiddleware) WrapOperation(operationName string, operation func() error) error {
	start := time.Now()

	lm.logger.Debug("Operation started", "operation", operationName)

	err := operation()
	duration := time.Since(start)

	if err != nil {
		lm.logger.LogError(err, "Operation failed",
			"operation", operationName,
			"duration_ms", duration.Milliseconds())
		return err
	}

	lm.logger.LogPerformance(operationName, duration)
	lm.logger.Debug("Operation completed successfully",
		"operation", operationName,
		"duration_ms", duration.Milliseconds())

	return nil
}

// WrapOperationWithContext wraps an operation with context-aware logging
func (lm *LoggingMiddleware) WrapOperationWithContext(ctx context.Context, operationName string, operation func(context.Context) error) error {
	start := time.Now()

	lm.logger.DebugWithContext(ctx, "Operation started", "operation", operationName)

	err := operation(ctx)
	duration := time.Since(start)

	if err != nil {
		lm.logger.ErrorWithContext(ctx, "Operation failed",
			"operation", operationName,
			"duration_ms", duration.Milliseconds(),
			"error", err.Error())
		return err
	}

	lm.logger.LogPerformance(operationName, duration)
	lm.logger.DebugWithContext(ctx, "Operation completed successfully",
		"operation", operationName,
		"duration_ms", duration.Milliseconds())

	return nil
}
