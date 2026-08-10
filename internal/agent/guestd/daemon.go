package guestd

import (
	"flag"
	"os"
	"strings"
	"sync"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	ServerPort     = ":4064"
	vsockReadyPort = 4065
)

var (
	currentSandboxID string
	rootLogger       ulog.Logger
	mu               sync.Mutex
)

func initDefaultLogger(logFile string) ulog.Logger {
	config := ulog.Config{
		Level:  ulog.InfoLevel,
		Stdout: true,
	}
	if logFile != "" {
		config.OutputFile = logFile
	}
	if err := ulog.Init(config); err != nil {
		panic(err)
	}

	logger := ulog.GetLogger()
	if currentSandboxID != "" {
		logger = logger.With(ulog.F("sandboxId", currentSandboxID))
		ulog.SetLogger(logger)
	}
	rootLogger = logger
	return logger
}

func refreshSandboxLoggerFromCmdline() string {
	sandboxID := getSandboxIDFromCmdline()
	if sandboxID == "" {
		return ""
	}

	mu.Lock()
	currentSandboxID = sandboxID
	mu.Unlock()

	if rootLogger == nil {
		rootLogger = ulog.GetLogger()
	}
	rootLogger = rootLogger.ReplaceField("sandboxId", sandboxID)
	ulog.SetLogger(rootLogger)
	return sandboxID
}

func getSandboxIDFromCmdline() string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	for _, field := range strings.Fields(string(data)) {
		if strings.HasPrefix(field, "conch.sandbox_id=") {
			return strings.TrimPrefix(field, "conch.sandbox_id=")
		}
	}
	return ""
}

func Run(args []string) error {
	flags := flag.NewFlagSet("conch-init", flag.ExitOnError)
	flags.Bool("init", true, "Run as init process (PID 1)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	initDefaultLogger("")
	runAsInit()
	return nil
}
