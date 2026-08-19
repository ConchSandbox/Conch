package main

import (
	"flag"
	"fmt"
	"os"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/daemon"
	"github.com/openeuler/Conch/internal/util"
	"github.com/openeuler/Conch/internal/version"
	"github.com/openeuler/Conch/pkg/ulog"
)

func main() {
	// Parse command line flags
	configPath := flag.String("config", config.FindConfigFile(), "Path to config file")
	showVersionShort := flag.Bool("v", false, "Print version information")
	showVersion := flag.Bool("version", false, "Print version information")
	flag.Parse()

	if *showVersionShort || *showVersion {
		fmt.Println(version.String())
		return
	}

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
	paths := cfg.RuntimePaths()
	if err := util.WritePIDFile(paths.ConchdPIDFile); err != nil {
		logger.Fatal("Failed to acquire pid file", ulog.F("error", err))
	}
	defer util.RemovePIDFile(paths.ConchdPIDFile)

	logger.Info("Parsed cli flags",
		ulog.F("configPath", *configPath),
	)
	logger.Info("Loaded configuration",
		ulog.F("config.path", cfgPath),
		ulog.F("app", cfg.App.Name),
		ulog.F("log.level", cfg.Log.Level),
		ulog.F("log.output", cfg.Log.Output),
		ulog.F("server.work_dir", cfg.Server.WorkDir),
		ulog.F("server.unix_socket", paths.ConchdSocket),
		ulog.F("server.state_dir", cfg.Server.StateDir),
		ulog.F("containerd.namespace", containerdclient.Namespace),
		ulog.F("network.warm_pool_size", cfg.Network.WarmPoolSize),
	)

	server, err := daemon.New(cfg)
	if err != nil {
		logger.Fatal("Failed to initialize server", ulog.F("error", err))
	}
	defer server.Cleanup()

	serverUnixSocket := paths.ConchdSocket
	logger.Info("Starting conchd server", ulog.F("network", "unix"), ulog.F("socket", serverUnixSocket))
	if err := server.Start(serverUnixSocket); err != nil {
		logger.Error("Failed to start server", ulog.F("error", err))
		server.Cleanup()
		os.Exit(1)
	}
}
