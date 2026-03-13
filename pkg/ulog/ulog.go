package ulog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxFileSize is the default maximum size for a log file (10MB)
	DefaultMaxFileSize = 10 * 1024 * 1024
)

// LogLevel defines the severity level for logging
type LogLevel int

const (
	// DebugLevel logs are typically voluminous, and are usually disabled in production
	DebugLevel LogLevel = iota
	// InfoLevel is the default logging priority
	InfoLevel
	// WarnLevel logs are more important than Info, but don't need individual human review
	WarnLevel
	// ErrorLevel logs are high-priority. If an application is running smoothly,
	// it shouldn't generate any error-level logs.
	ErrorLevel
	// FatalLevel logs a message, then exits. os.Exit(1) is called
	FatalLevel
)

// String returns the string representation of the log level
func (l LogLevel) String() string {
	switch l {
	case DebugLevel:
		return "DEBUG"
	case InfoLevel:
		return "INFO"
	case WarnLevel:
		return "WARN"
	case ErrorLevel:
		return "ERROR"
	case FatalLevel:
		return "FATAL"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", l)
	}
}

// Logger is the main logging interface
type Logger interface {
	Debug(msg string, fields ...Field)
	Info(msg string, fields ...Field)
	Warn(msg string, fields ...Field)
	Error(msg string, fields ...Field)
	Fatal(msg string, fields ...Field)
	With(fields ...Field) Logger
	WithContext(ctx context.Context) Logger
}

// Field represents a key-value pair for structured logging
type Field struct {
	Key   string
	Value interface{}
}

// F is a helper function to create a Field
func F(key string, value interface{}) Field {
	return Field{Key: key, Value: value}
}

// ulog implements the Logger interface
type ulog struct {
	mu          sync.Mutex
	level       LogLevel
	output      *os.File
	filePath    string
	writer      writer
	fields      []Field
	maxFileSize int64
	rotation    int
}

// writeSyncer wraps a file and provides synchronized writes
type writeSyncer struct {
	file *os.File
}

// writer defines the interface for writers that support Sync
type writer interface {
	Write([]byte) (int, error)
	Sync() error
}

func (w *writeSyncer) Write(p []byte) (n int, err error) {
	return w.file.Write(p)
}

func (w *writeSyncer) Sync() error {
	return w.file.Sync()
}

// stdoutWriter writes only to stdout
type stdoutWriter struct{}

func (w *stdoutWriter) Write(p []byte) (n int, err error) {
	return os.Stdout.Write(p)
}

func (w *stdoutWriter) Sync() error {
	return nil // stdout doesn't need sync
}

// Config holds the configuration for the logger
type Config struct {
	Level       LogLevel // Minimum log level to output
	OutputPath  string   // Path to log directory (default: /var/log/conchd/)
	Stdout      bool     // Also write to stdout
	MaxFileSize int64    // Maximum size for a log file before rotation (default: 10MB)
}

var (
	// global logger instance
	defaultLogger Logger

	// default config
	defaultConfig = Config{
		Level:       InfoLevel,
		OutputPath:  "/var/log/conchd/",
		Stdout:      true,
		MaxFileSize: DefaultMaxFileSize,
	}
)

func newStdoutLogger(level LogLevel, maxFileSize int64) Logger {
	if maxFileSize == 0 {
		maxFileSize = DefaultMaxFileSize
	}
	return &ulog{
		level:       level,
		output:      nil,
		filePath:    "",
		writer:      &stdoutWriter{},
		maxFileSize: maxFileSize,
		rotation:    0,
	}
}

// Init initializes the global logger with the given config
func Init(config Config) error {
	if config.Level == 0 {
		config.Level = defaultConfig.Level
	}
	if config.MaxFileSize == 0 {
		config.MaxFileSize = defaultConfig.MaxFileSize
	}

	logger := &ulog{
		level:       config.Level,
		maxFileSize: config.MaxFileSize,
		rotation:    0,
	}

	// Determine output mode based on OutputPath and Stdout
	// Mode 1: Only stdout (OutputPath is empty, Stdout is true)
	// Mode 2: Only file (OutputPath is set, Stdout is false)
	// Mode 3: Both stdout and file (OutputPath is set, Stdout is true)
	onlyStdout := (config.OutputPath == "" && config.Stdout)
	fileMode := (config.OutputPath != "")
	bothMode := (config.OutputPath != "" && config.Stdout)

	if onlyStdout {
		// Pure stdout mode - no file creation
		logger.output = nil
		logger.filePath = ""
		logger.writer = &stdoutWriter{}
	} else if fileMode {
		// File mode or both mode - create log directory and file
		// Create log directory if it doesn't exist
		if err := os.MkdirAll(config.OutputPath, 0755); err != nil {
			return fmt.Errorf("failed to create log directory: %w", err)
		}

		// Create log file with datetime in filename
		now := time.Now()
		datetime := now.Format("20060102-150405")
		logFileName := fmt.Sprintf("%s.log", datetime)
		logFilePath := filepath.Join(config.OutputPath, logFileName)

		logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to create log file: %w", err)
		}

		logger.output = logFile
		logger.filePath = logFilePath

		if bothMode {
			// Both mode - write to file and stdout
			logger.writer = &multiWriter{
				file:        logFile,
				stdout:      os.Stdout,
				filePath:    logFilePath,
				maxFileSize: config.MaxFileSize,
				baseName:    logFileName,
				outputPath:  config.OutputPath,
				rotation:    &logger.rotation,
			}
		} else {
			// File mode - write only to file
			logger.writer = &writeSyncer{file: logFile}
		}
	} else {
		// Fallback: OutputPath is empty but Stdout is false - use default file mode
		config.OutputPath = defaultConfig.OutputPath
		if err := os.MkdirAll(config.OutputPath, 0755); err != nil {
			return fmt.Errorf("failed to create log directory: %w", err)
		}

		now := time.Now()
		datetime := now.Format("20060102-150405")
		logFileName := fmt.Sprintf("%s.log", datetime)
		logFilePath := filepath.Join(config.OutputPath, logFileName)

		logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to create log file: %w", err)
		}

		logger.output = logFile
		logger.filePath = logFilePath
		logger.writer = &writeSyncer{file: logFile}
	}

	// Close old logger's file if it exists
	if defaultLogger != nil {
		if oldLogger, ok := defaultLogger.(*ulog); ok && oldLogger.output != nil {
			_ = oldLogger.output.Close()
		}
	}

	defaultLogger = logger
	return nil
}

// multiWriter writes to both file and stdout
type multiWriter struct {
	file        *os.File
	stdout      *os.File
	filePath    string
	maxFileSize int64
	baseName    string
	outputPath  string
	rotation    *int
	mu          sync.Mutex
}

func (w *multiWriter) Write(p []byte) (n int, err error) {
	// Check if we need to rotate before writing
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.checkAndRotate(); err != nil {
		return 0, err
	}

	n, err = w.file.Write(p)
	if err != nil {
		return
	}
	return w.stdout.Write(p)
}

// checkAndRotate checks if the current log file exceeds max size and rotates if needed
func (w *multiWriter) checkAndRotate() error {
	// Get current file size
	info, err := w.file.Stat()
	if err != nil {
		return err
	}

	// If file size is within limits, no rotation needed
	if info.Size() < w.maxFileSize {
		return nil
	}

	// Close current file
	_ = w.file.Sync()
	_ = w.file.Close()

	// Create new log file with rotation number
	*w.rotation++
	datetime := time.Now().Format("20060102-150405")
	baseName := strings.TrimSuffix(w.baseName, ".log")
	newFileName := fmt.Sprintf("%s.%d.log", baseName, *w.rotation)
	newFilePath := filepath.Join(w.outputPath, newFileName)

	// Replace base name with new datetime
	newBaseName := datetime + ".log"
	newFileName = fmt.Sprintf("%s.%d.log", newBaseName, *w.rotation)
	newFilePath = filepath.Join(w.outputPath, newFileName)

	// Open new log file
	newFile, err := os.OpenFile(newFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to create rotated log file: %w", err)
	}

	// Update file references
	w.file = newFile
	w.filePath = newFilePath

	return nil
}

func (w *multiWriter) Sync() error {
	return w.file.Sync()
}

// GetLogger returns the global logger instance
func GetLogger() Logger {
	if defaultLogger == nil {
		// Initialize with default config if not initialized
		if err := Init(defaultConfig); err != nil {
			defaultLogger = newStdoutLogger(defaultConfig.Level, defaultConfig.MaxFileSize)
			fmt.Fprintf(os.Stderr, "ulog: fallback to stdout logger: %v\n", err)
		}
	}
	return defaultLogger
}

// SetLogger sets the global logger instance (for testing or advanced usage)
func SetLogger(logger Logger) {
	defaultLogger = logger
}

// Package-level logging functions for convenience
// These allow direct calls like ulog.Debug() instead of ulog.GetLogger().Debug()

// Debug logs a message at DebugLevel
func Debug(msg string, fields ...Field) {
	GetLogger().Debug(msg, fields...)
}

// Info logs a message at InfoLevel
func Info(msg string, fields ...Field) {
	GetLogger().Info(msg, fields...)
}

// Warn logs a message at WarnLevel
func Warn(msg string, fields ...Field) {
	GetLogger().Warn(msg, fields...)
}

// Error logs a message at ErrorLevel
func Error(msg string, fields ...Field) {
	GetLogger().Error(msg, fields...)
}

// Fatal logs a message at FatalLevel then exits
func Fatal(msg string, fields ...Field) {
	GetLogger().Fatal(msg, fields...)
}

// With returns a new logger with additional fields
func With(fields ...Field) Logger {
	return GetLogger().With(fields...)
}

// WithContext returns a new logger with context fields
func WithContext(ctx context.Context) Logger {
	return GetLogger().WithContext(ctx)
}

// log formats and writes a log entry
func (l *ulog) log(level LogLevel, msg string, fields ...Field) {
	if l.level > level {
		return
	}

	// Build the log message
	var b strings.Builder

	// Timestamp
	now := time.Now()
	b.WriteString(now.Format("2006-01-02 15:04:05.000"))
	b.WriteString(" ")

	// Level
	b.WriteString("[")
	b.WriteString(level.String())
	b.WriteString("] ")

	// Source (file:line)
	if _, file, line, ok := caller(3); ok {
		b.WriteString(filepath.Base(file))
		b.WriteString(":")
		b.WriteString(fmt.Sprintf("%d", line))
		b.WriteString(" ")
	}

	// Message
	b.WriteString(msg)

	// Fields
	if len(fields) > 0 || len(l.fields) > 0 {
		allFields := make([]Field, 0, len(fields)+len(l.fields))
		allFields = append(allFields, l.fields...)
		allFields = append(allFields, fields...)
		b.WriteString(" ")
		for i, f := range allFields {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(fmt.Sprintf("%s=%v", f.Key, f.Value))
		}
	}

	b.WriteString("\n")

	// Write the log entry
	l.mu.Lock()
	defer l.mu.Unlock()

	// Check if we need to rotate (for writeSyncer mode, multiWriter handles its own rotation)
	if l.maxFileSize > 0 {
		if ws, ok := l.writer.(*writeSyncer); ok {
			if info, err := l.output.Stat(); err == nil {
				if info.Size() >= l.maxFileSize {
					// Rotate the log file
					_ = l.writer.Sync()
					_ = l.output.Close()

					// Create new log file with rotation number
					l.rotation++
					datetime := time.Now().Format("20060102-150405")
					ext := filepath.Ext(l.filePath)
					dir := filepath.Dir(l.filePath)

					// Use datetime for new log file name
					newBaseName := datetime + ext
					newFilePath := filepath.Join(dir, fmt.Sprintf("%s.%d", newBaseName, l.rotation))

					// Open new log file
					newFile, err := os.OpenFile(newFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
					if err != nil {
						fmt.Fprintf(os.Stderr, "failed to create rotated log file: %v\n", err)
						// Try to reopen the old file
						if l.output, err = os.OpenFile(l.filePath, os.O_WRONLY|os.O_APPEND, 0644); err == nil {
							l.writer = &writeSyncer{file: l.output}
						}
					} else {
						l.output = newFile
						l.filePath = newFilePath
						l.writer = &writeSyncer{file: newFile}
						ws.file = newFile
					}
				}
			}
		}
	}

	if _, err := l.writer.Write([]byte(b.String())); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write log: %v\n", err)
	}

	// Sync to ensure logs are written
	_ = l.writer.Sync()

	// If fatal level, exit
	if level == FatalLevel {
		os.Exit(1)
	}
}

// Debug logs a message at DebugLevel
func (l *ulog) Debug(msg string, fields ...Field) {
	l.log(DebugLevel, msg, fields...)
}

// Info logs a message at InfoLevel
func (l *ulog) Info(msg string, fields ...Field) {
	l.log(InfoLevel, msg, fields...)
}

// Warn logs a message at WarnLevel
func (l *ulog) Warn(msg string, fields ...Field) {
	l.log(WarnLevel, msg, fields...)
}

// Error logs a message at ErrorLevel
func (l *ulog) Error(msg string, fields ...Field) {
	l.log(ErrorLevel, msg, fields...)
}

// Fatal logs a message at FatalLevel then exits
func (l *ulog) Fatal(msg string, fields ...Field) {
	l.log(FatalLevel, msg, fields...)
}

// With returns a new logger with additional fields
func (l *ulog) With(fields ...Field) Logger {
	newLogger := &ulog{
		level:    l.level,
		output:   l.output,
		filePath: l.filePath,
		writer:   l.writer,
	}
	newLogger.fields = append([]Field{}, l.fields...)
	newLogger.fields = append(newLogger.fields, fields...)
	return newLogger
}

// WithContext returns a new logger with context fields
func (l *ulog) WithContext(ctx context.Context) Logger {
	newLogger := &ulog{
		level:    l.level,
		output:   l.output,
		filePath: l.filePath,
		writer:   l.writer,
	}
	newLogger.fields = append([]Field{}, l.fields...)

	// Add common context fields
	if requestID := ctx.Value("request_id"); requestID != nil {
		newLogger.fields = append(newLogger.fields, F("request_id", requestID))
	}
	if userID := ctx.Value("user_id"); userID != nil {
		newLogger.fields = append(newLogger.fields, F("user_id", userID))
	}
	if traceID := ctx.Value("trace_id"); traceID != nil {
		newLogger.fields = append(newLogger.fields, F("trace_id", traceID))
	}

	return newLogger
}

// caller returns the file, line, and function name of the caller
func caller(skip int) (uintptr, string, int, bool) {
	pc, file, line, ok := runtime.Caller(skip)
	return pc, file, line, ok
}

// Close closes the log file
func (l *ulog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.output == nil {
		return nil // Nothing to close in pure stdout mode
	}
	return l.output.Close()
}

// GetFilePath returns the current log file path
func (l *ulog) GetFilePath() string {
	return l.filePath
}

// SetLevel sets the log level
func (l *ulog) SetLevel(level LogLevel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// GetLevel returns the current log level
func (l *ulog) GetLevel() LogLevel {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}
