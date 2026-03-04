# ulog - Conch 统一日志模块

ulog 是 Conch 项目的统一日志模块，提供结构化日志记录功能。

## 特性

- 结构化日志输出
- 支持多种日志级别（Debug, Info, Warn, Error, Fatal）
- 支持文件输出（默认：`/var/log/conchd/<datetime>.log`）
- 支持同时输出到 stdout
- 支持字段追加（Field）
- 支持 Context 上下文字段
- 线程安全
- 自动记录调用位置（文件:行号）

## 安装

```go
import "github.com/openeuler/Conch/pkg/ulog"
```

## 使用示例

### 初始化

```go
package main

import (
    "github.com/openeuler/Conch/pkg/ulog"
)

func main() {
    // 使用默认配置初始化
    err := ulog.Init(ulog.Config{})
    if err != nil {
        panic(err)
    }

    // 或者使用自定义配置
    err = ulog.Init(ulog.Config{
        Level:      ulog.DebugLevel,
        OutputPath: "/var/log/conchd/",
        Stdout:     true,
    })
    if err != nil {
        panic(err)
    }

    logger := ulog.GetLogger()
    logger.Info("Application started")
}
```

### 基本日志

```go
logger := ulog.GetLogger()

logger.Debug("Debug message")
logger.Info("Info message")
logger.Warn("Warning message")
logger.Error("Error message")
logger.Fatal("Fatal message - will exit")
```

### 带字段的日志

```go
logger := ulog.GetLogger()

logger.Info("User logged in",
    ulog.F("user_id", 12345),
    ulog.F("ip", "192.168.1.1"),
    ulog.F("method", "oauth"),
)

logger.Error("Failed to create sandbox",
    ulog.F("sandbox_id", "sandbox_001"),
    ulog.F("error", "timeout"),
)
```

### 字段追加（With）

```go
logger := ulog.GetLogger()

// 创建带有固定字段的 logger
baseLogger := logger.With(
    ulog.F("service", "conchd"),
    ulog.F("version", "1.0.0"),
)

// 使用带有固定字段的 logger
baseLogger.Info("Processing request",
    ulog.F("request_id", "req_123"),
)

baseLogger.Error("Request failed",
    ulog.F("request_id", "req_123"),
    ulog.F("error", "invalid input"),
)
```

### Context 上下文日志

```go
import (
    "context"
    "github.com/openeuler/Conch/pkg/ulog"
)

func handler(ctx context.Context) {
    // 添加 context 信息
    ctx = context.WithValue(ctx, "request_id", "req_123")
    ctx = context.WithValue(ctx, "user_id", 456)

    logger := ulog.GetLogger().WithContext(ctx)
    logger.Info("Processing request")
}
```

### 日志级别控制

```go
logger := ulog.GetLogger()

// 设置日志级别
logger.SetLevel(ulog.DebugLevel)

// 获取当前日志级别
level := logger.GetLevel()
fmt.Println("Current log level:", level)
```

### 关闭日志文件

```go
logger := ulog.GetLogger()
defer logger.Close()
```

## 日志格式

```
2006-01-02 15:04:05.000 [LEVEL] file:line message key1=value1 key2=value2
```

示例：

```
2024-03-03 14:30:45.123 [INFO] main.go:25 Application started service=conchd version=1.0.0
2024-03-03 14:30:46.456 [ERROR] sandbox.go:123 Failed to create sandbox sandbox_id=sandbox_001 error=timeout
```

## 配置选项

```go
type Config struct {
    Level      LogLevel  // 日志级别（默认：InfoLevel）
    OutputPath string   // 日志目录（默认：/var/log/conchd/）
    Stdout     bool      // 是否输出到 stdout（默认：true）
}
```

### 日志级别

- `ulog.DebugLevel` - 调试级别
- `ulog.InfoLevel` - 信息级别（默认）
- `ulog.WarnLevel` - 警告级别
- `ulog.ErrorLevel` - 错误级别
- `ulog.FatalLevel` - 致命级别（记录后退出程序）

## 迁移指南

### 从 fmt.Println 迁移

**之前：**
```go
fmt.Println("Processing request")
fmt.Printf("Sandbox created: %s\n", sandboxID)
```

**之后：**
```go
logger := ulog.GetLogger()
logger.Info("Processing request")
logger.Info("Sandbox created", ulog.F("sandbox_id", sandboxID))
```

### 从 log.Println 迁移

**之前：**
```go
import "log"

log.Println("Server starting")
log.Printf("Listening on %s\n", addr)
```

**之后：**
```go
logger := ulog.GetLogger()
logger.Info("Server starting")
logger.Info("Listening", ulog.F("address", addr))
```

### 从 slog 迁移（如需统一）

**之前：**
```go
import "log/slog"

slog.Info("Processing", "request_id", reqID)
```

**之后：**
```go
logger := ulog.GetLogger()
logger.Info("Processing", ulog.F("request_id", reqID))
```

## 注意事项

1. **初始化顺序**：确保在程序开始时调用 `ulog.Init()`
2. **日志文件命名**：日志文件按创建时间命名（`YYYYMMDD-HHMMSS.log`）
3. **日志目录权限**：确保程序有权限写入 `/var/log/conchd/` 目录
4. ** Fatal 日志**：使用 `Fatal()` 后程序会退出，不需要额外调用 `os.Exit()`
5. **并发安全**：Logger 是并发安全的，可以在多个 goroutine 中使用
