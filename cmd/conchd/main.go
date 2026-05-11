package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/openeuler/Conch/internal"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/util"
	"github.com/openeuler/Conch/pkg/ulog"
)

func main() {
	// Parse command line flags
	configPath := flag.String("config", config.FindConfigFile(), "Path to config file")
	flag.Parse()

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = config.FindConfigFile()
	}
	if cfgPath != "" {
		fmt.Printf("Using config: %s\n", cfgPath)
	}

	// Load configuration
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize the process-wide logger from config once, and keep it alive for the full
	// daemon lifetime so background goroutines do not race with a closed writer.
	logConfig, err := cfg.GetLogConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid log config: %v\n", err)
		os.Exit(1)
	}

	if err := ulog.Init(logConfig); err != nil {
		ulog.GetLogger().Fatal("Failed to initialize logger", ulog.F("error", err))
	}

	logger := ulog.GetLogger()
	if err := util.WritePIDFile(cfg.Server.PIDFile); err != nil {
		logger.Fatal("Failed to acquire pid file", ulog.F("error", err))
	}
	defer util.RemovePIDFile(cfg.Server.PIDFile)

	logger.Info("Parsed cli flags",
		ulog.F("configPath", *configPath),
	)
	logger.Info("Loaded configuration",
		ulog.F("config.path", cfgPath),
		ulog.F("app", cfg.App.Name),
		ulog.F("log.level", cfg.Log.Level),
		ulog.F("log.output", cfg.Log.Output),
		ulog.F("server.address", cfg.GetServerAddress()),
		ulog.F("server.work_dir", cfg.Server.WorkDir),
		ulog.F("server.unix_socket", cfg.GetServerUnixSocket()),
		ulog.F("server.pid_file", cfg.Server.PIDFile),
		ulog.F("containerd.socket", cfg.Containerd.Socket),
		ulog.F("containerd.root_dir", cfg.Containerd.RootDir),
		ulog.F("containerd.state_dir", cfg.Containerd.StateDir),
		ulog.F("containerd.default_namespace", cfg.Containerd.DefaultNamespace),
		ulog.F("network.pool_size", cfg.Network.PoolSize),
	)

	server, err := internal.NewServer(cfg)
	if err != nil {
		logger.Fatal("Failed to initialize server", ulog.F("error", err))
	}
	defer server.Cleanup()

	serverAddr := cfg.GetServerAddress()
	serverUnixSocket := cfg.GetServerUnixSocket()
	if serverUnixSocket != "" {
		logger.Info("Starting conchd server", ulog.F("network", "unix"), ulog.F("socket", serverUnixSocket))
	} else {
		logger.Info("Starting conchd server", ulog.F("network", "tcp"), ulog.F("address", serverAddr))
	}
	if err := server.Start(serverAddr, serverUnixSocket); err != nil {
		logger.Error("Failed to start server", ulog.F("error", err))
		server.Cleanup()
		os.Exit(1)
	}
}
