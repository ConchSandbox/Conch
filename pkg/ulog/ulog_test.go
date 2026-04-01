package ulog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogLevelString(t *testing.T) {
	tests := []struct {
		level    LogLevel
		expected string
	}{
		{DebugLevel, "DEBUG"},
		{InfoLevel, "INFO"},
		{WarnLevel, "WARN"},
		{ErrorLevel, "ERROR"},
		{FatalLevel, "FATAL"},
		{LogLevel(99), "UNKNOWN(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.level.String(); got != tt.expected {
				t.Errorf("LogLevel.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestInit(t *testing.T) {
	// Create a temp directory for logs
	tmpDir := t.TempDir()

	err := Init(Config{
		Level:      DebugLevel,
		OutputPath: tmpDir,
		Stdout:     false,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	logger := GetLogger()
	if logger == nil {
		t.Fatal("GetLogger() returned nil")
	}
	if got, ok := logger.(*ulog); ok {
		if got.GetLevel() != DebugLevel {
			t.Fatalf("Init() level = %v, want %v", got.GetLevel(), DebugLevel)
		}
	}

	// Check if log file was created
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read log directory: %v", err)
	}

	if len(entries) == 0 {
		t.Fatal("No log file was created")
	}

	// Check log file name format
	logFileName := entries[0].Name()
	if !strings.HasSuffix(logFileName, ".log") {
		t.Errorf("Log file name doesn't end with .log: %s", logFileName)
	}

	// Cleanup
	if closer, ok := logger.(*ulog); ok {
		_ = closer.Close()
	}
}

func TestGetLoggerDefaultsToInfoLevel(t *testing.T) {
	SetLogger(nil)

	logger := GetLogger()
	ulogger, ok := logger.(*ulog)
	if !ok {
		t.Fatalf("GetLogger() type = %T, want *ulog", logger)
	}
	if ulogger.GetLevel() != InfoLevel {
		t.Fatalf("GetLogger() default level = %v, want %v", ulogger.GetLevel(), InfoLevel)
	}

	_ = ulogger.Close()
}

func TestLoggerMethods(t *testing.T) {
	tmpDir := t.TempDir()

	err := Init(Config{
		Level:      DebugLevel,
		OutputPath: tmpDir,
		Stdout:     false,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	logger := GetLogger()

	// Test all log methods
	logger.Debug("Debug message")
	logger.Info("Info message")
	logger.Warn("Warn message")
	logger.Error("Error message")

	// Read log file to verify content
	entries, _ := os.ReadDir(tmpDir)
	logFilePath := filepath.Join(tmpDir, entries[0].Name())

	content, err := os.ReadFile(logFilePath)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	logContent := string(content)

	// Verify all levels are present
	if !strings.Contains(logContent, "[DEBUG]") {
		t.Error("Log doesn't contain DEBUG level")
	}
	if !strings.Contains(logContent, "[INFO]") {
		t.Error("Log doesn't contain INFO level")
	}
	if !strings.Contains(logContent, "[WARN]") {
		t.Error("Log doesn't contain WARN level")
	}
	if !strings.Contains(logContent, "[ERROR]") {
		t.Error("Log doesn't contain ERROR level")
	}

	// Verify messages are present
	if !strings.Contains(logContent, "Debug message") {
		t.Error("Log doesn't contain debug message")
	}
	if !strings.Contains(logContent, "Info message") {
		t.Error("Log doesn't contain info message")
	}
	if !strings.Contains(logContent, "Warn message") {
		t.Error("Log doesn't contain warn message")
	}
	if !strings.Contains(logContent, "Error message") {
		t.Error("Log doesn't contain error message")
	}

	// Cleanup
	if closer, ok := logger.(*ulog); ok {
		_ = closer.Close()
	}
}

func TestLoggerWithFields(t *testing.T) {
	tmpDir := t.TempDir()

	err := Init(Config{
		Level:      InfoLevel,
		OutputPath: tmpDir,
		Stdout:     false,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	logger := GetLogger()

	// Test logging with fields
	logger.Info("Test message",
		F("key1", "value1"),
		F("key2", 123),
		F("key3", true),
	)

	// Read log file
	entries, _ := os.ReadDir(tmpDir)
	logFilePath := filepath.Join(tmpDir, entries[0].Name())
	content, _ := os.ReadFile(logFilePath)

	logContent := string(content)

	// Verify fields are present
	if !strings.Contains(logContent, "key1=value1") {
		t.Error("Log doesn't contain key1 field")
	}
	if !strings.Contains(logContent, "key2=123") {
		t.Error("Log doesn't contain key2 field")
	}
	if !strings.Contains(logContent, "key3=true") {
		t.Error("Log doesn't contain key3 field")
	}

	// Cleanup
	if closer, ok := logger.(*ulog); ok {
		_ = closer.Close()
	}
}

func TestLoggerWith(t *testing.T) {
	tmpDir := t.TempDir()

	err := Init(Config{
		Level:      InfoLevel,
		OutputPath: tmpDir,
		Stdout:     false,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	logger := GetLogger()

	// Create a logger with base fields
	baseLogger := logger.With(
		F("service", "conchd"),
		F("version", "1.0.0"),
	)

	// Log with base fields
	baseLogger.Info("Message 1")
	baseLogger.Error("Message 2")

	// Read log file
	entries, _ := os.ReadDir(tmpDir)
	logFilePath := filepath.Join(tmpDir, entries[0].Name())
	content, _ := os.ReadFile(logFilePath)

	logContent := string(content)

	// Verify base fields are present in both log entries
	lines := strings.Split(logContent, "\n")
	if len(lines) < 2 {
		t.Fatal("Not enough log lines")
	}

	// Check first line (Info)
	if !strings.Contains(lines[0], "service=conchd") {
		t.Error("First log doesn't contain service field")
	}
	if !strings.Contains(lines[0], "version=1.0.0") {
		t.Error("First log doesn't contain version field")
	}
	if !strings.Contains(lines[0], "[INFO]") {
		t.Error("First log doesn't contain INFO level")
	}

	// Check second line (Error)
	if !strings.Contains(lines[1], "service=conchd") {
		t.Error("Second log doesn't contain service field")
	}
	if !strings.Contains(lines[1], "version=1.0.0") {
		t.Error("Second log doesn't contain version field")
	}
	if !strings.Contains(lines[1], "[ERROR]") {
		t.Error("Second log doesn't contain ERROR level")
	}

	// Cleanup
	if closer, ok := logger.(*ulog); ok {
		_ = closer.Close()
	}
}

func TestLoggerLevelFiltering(t *testing.T) {
	tmpDir := t.TempDir()

	err := Init(Config{
		Level:      WarnLevel, // Only log WARN and above
		OutputPath: tmpDir,
		Stdout:     false,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	logger := GetLogger()

	// Log at different levels
	logger.Debug("Debug message - should not appear")
	logger.Info("Info message - should not appear")
	logger.Warn("Warn message - should appear")
	logger.Error("Error message - should appear")

	// Read log file
	entries, _ := os.ReadDir(tmpDir)
	logFilePath := filepath.Join(tmpDir, entries[0].Name())
	content, _ := os.ReadFile(logFilePath)

	logContent := string(content)

	// Verify only WARN and ERROR are present
	if strings.Contains(logContent, "[DEBUG]") {
		t.Error("Log contains DEBUG level (should be filtered)")
	}
	if strings.Contains(logContent, "[INFO]") {
		t.Error("Log contains INFO level (should be filtered)")
	}
	if !strings.Contains(logContent, "[WARN]") {
		t.Error("Log doesn't contain WARN level")
	}
	if !strings.Contains(logContent, "[ERROR]") {
		t.Error("Log doesn't contain ERROR level")
	}

	// Cleanup
	if closer, ok := logger.(*ulog); ok {
		_ = closer.Close()
	}
}

func TestLoggerSetLevel(t *testing.T) {
	tmpDir := t.TempDir()

	err := Init(Config{
		Level:      ErrorLevel,
		OutputPath: tmpDir,
		Stdout:     false,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	logger := GetLogger()
	if ulog, ok := logger.(*ulog); ok {
		// Check initial level
		if ulog.GetLevel() != ErrorLevel {
			t.Errorf("Initial level = %v, want %v", ulog.GetLevel(), ErrorLevel)
		}

		// Change level
		ulog.SetLevel(DebugLevel)
		if ulog.GetLevel() != DebugLevel {
			t.Errorf("Updated level = %v, want %v", ulog.GetLevel(), DebugLevel)
		}

		// Cleanup
		_ = ulog.Close()
	}
}

func TestField(t *testing.T) {
	field := F("key", "value")
	if field.Key != "key" {
		t.Errorf("Field.Key = %v, want key", field.Key)
	}
	if field.Value != "value" {
		t.Errorf("Field.Value = %v, want value", field.Value)
	}
}
