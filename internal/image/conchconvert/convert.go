package conchconvert

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/image/client"
	"github.com/sirupsen/logrus"
)

type ConvertOpts struct {
	Source           string
	KernelPath       string
	InitrdPath       string
	BootIndexTag     string
	Namespace        string
	ConfigPath       string
	ConchAPIBaseURL  string
	PlainHTTP        bool
	Username         string
	Password         string
	Snapshot         bool
	Out              io.Writer
	KeepDebugArchive bool
}

type Result struct {
	BootIndexDigest string
	BootIndexTag    string
	RootfsImageRef  string
	KernelImageRef  string
	SourceImageRef  string
}

type SnapshotExportOpts struct {
	BootIndexTag     string
	ConfigPath       string
	ConchAPIBaseURL  string
	RootfsSnapshotID string
	SandboxID        string
}

func Convert(ctx context.Context, opts ConvertOpts) (Result, error) {
	if strings.TrimSpace(opts.Source) == "" {
		return Result{}, fmt.Errorf("--source is required")
	}
	if strings.TrimSpace(opts.BootIndexTag) == "" {
		return Result{}, fmt.Errorf("output image tag is required")
	}
	kernelPath, err := regularFilePath(opts.KernelPath, "kernel")
	if err != nil {
		return Result{}, err
	}
	initrdPath, err := regularFilePath(opts.InitrdPath, "initrd")
	if err != nil {
		return Result{}, err
	}

	conchClient := client.NewClientWithConfig(opts.ConchAPIBaseURL, opts.ConfigPath)
	resp, err := conchClient.ConvertImage(ctx, client.ConvertImageRequest{
		Source:       opts.Source,
		KernelPath:   kernelPath,
		InitrdPath:   initrdPath,
		BootIndexTag: opts.BootIndexTag,
		Namespace:    resolveConchNamespace(opts.ConfigPath, opts.Namespace),
		PlainHTTP:    opts.PlainHTTP,
		Username:     opts.Username,
		Password:     opts.Password,
		Snapshot:     opts.Snapshot,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{
		BootIndexDigest: resp.BootIndexDigest,
		BootIndexTag:    resp.BootIndexTag,
		RootfsImageRef:  resp.RootfsImageRef,
		KernelImageRef:  resp.KernelImageRef,
		SourceImageRef:  resp.SourceImageRef,
	}, nil
}

func ExportSnapshot(ctx context.Context, opts SnapshotExportOpts) (Result, error) {
	if (strings.TrimSpace(opts.RootfsSnapshotID) == "") == (strings.TrimSpace(opts.SandboxID) == "") {
		return Result{}, fmt.Errorf("exactly one of --snapshot-id or --sandbox-id is required")
	}
	conchClient := client.NewClientWithConfig(opts.ConchAPIBaseURL, opts.ConfigPath)
	resp, err := conchClient.ExportSnapshot(ctx, client.SnapshotExportRequest{
		Namespace:        resolveConchNamespace(opts.ConfigPath, ""),
		BootIndexTag:     opts.BootIndexTag,
		RootfsSnapshotID: opts.RootfsSnapshotID,
		SandboxID:        opts.SandboxID,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{
		BootIndexDigest: resp.BootIndexDigest,
		BootIndexTag:    resp.BootIndexTag,
	}, nil
}

func resolveConchNamespace(configPath, override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	namespace := ""
	cfgPath := configPath
	if cfgPath == "" {
		cfgPath = config.FindConfigFile()
	}
	if cfg, err := config.LoadConfig(cfgPath); err == nil {
		if cfgPath != "" {
			logrus.Infof("Using config: %s", cfgPath)
		}
		namespace = strings.TrimSpace(cfg.Containerd.DefaultNamespace)
	}
	if namespace == "" {
		namespace = "default"
	}
	return namespace
}

func regularFilePath(path, label string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s file path is required", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%s file path: %w", label, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s file %q: %w", label, abs, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s file %q is not a regular file", label, abs)
	}
	return abs, nil
}
