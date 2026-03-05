package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/openeuler/Conch/internal"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/pkg/ulog"
)

func main() {
	// Parse command line flags
	configPath := flag.String("config", "", "Path to config file")
	flag.Parse()

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger with config from config file
	logConfig, err := cfg.GetLogConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid log config: %v\n", err)
		os.Exit(1)
	}

	if err := ulog.Init(logConfig); err != nil {
		ulog.GetLogger().Fatal("Failed to initialize logger", ulog.F("error", err))
	}
	defer func() {
		logger := ulog.GetLogger()
		if closer, ok := logger.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	logger := ulog.GetLogger()
	logger.Info("Loaded configuration",
		ulog.F("app", cfg.App.Name),
		ulog.F("log.level", cfg.Log.Level),
		ulog.F("log.output", cfg.Log.Output),
		ulog.F("server.address", cfg.GetServerAddress()),
	)

	server, err := internal.NewServer()
	if err != nil {
		logger.Fatal("Failed to initialize server", ulog.F("error", err))
	}
	defer server.Cleanup()

	serverAddr := cfg.GetServerAddress()
	logger.Info("Starting conchd server", ulog.F("address", serverAddr))
	if err := server.Start(serverAddr); err != nil {
		logger.Fatal("Failed to start server", ulog.F("error", err))

	}
}
