package main

import (
	"github.com/openeuler/Conch/internal"
	"github.com/openeuler/Conch/pkg/ulog"
)

const listenPort = "4063"

func main() {
	// Initialize logger
	err := ulog.Init(ulog.Config{
		Level:      ulog.InfoLevel,
		OutputPath: "/var/log/conchd/",
		Stdout:     true,
	})
	if err != nil {
		ulog.GetLogger().Fatal("Failed to initialize logger", ulog.F("error", err))
	}
	defer func() {
		logger := ulog.GetLogger()
		if closer, ok := logger.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	logger := ulog.GetLogger()

	server, err := internal.NewServer()
	if err != nil {
		logger.Fatal("Failed to initialize server", ulog.F("error", err))
	}
	defer server.Cleanup()

	logger.Info("Starting conchd server", ulog.F("port", ":4063"))
	if err := server.Start(":4063"); err != nil {
		logger.Fatal("Failed to start server", ulog.F("error", err))

	}
}
