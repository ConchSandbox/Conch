package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"

	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/pkg/ulog"
)

const defaultContainerdAddress = "/run/containerd/containerd.sock"

func main() {
	// Initialize logger
	err := ulog.Init(ulog.Config{
		Level:      ulog.InfoLevel,
		OutputPath: "/var/log/conch-unpack/",
		Stdout:     true,
	})
	if err != nil {
		panic(err)
	}
	defer func() {
		logger := ulog.GetLogger()
		if closer, ok := logger.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	logger := ulog.GetLogger()

	addr := flag.String("address", "", "containerd socket address (default: "+defaultContainerdAddress+")")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <image-name>\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nExample: conch-unpack hub.oepkgs.net/conch/conch-index:v0.1")
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}
	imageName := flag.Arg(0)

	containerdAddr := defaultContainerdAddress
	if v := os.Getenv("CONTAINERD_ADDRESS"); v != "" {
		containerdAddr = v
	}
	if *addr != "" {
		containerdAddr = *addr
	}

	ctx := namespaces.WithNamespace(context.Background(), "default")
	client, err := containerd.New(containerdAddr)
	if err != nil {
		logger.Fatal("Failed to connect to containerd", ulog.F("error", err))
	}
	defer client.Close()

	logger.Info("Starting analysis for main image", ulog.F("image", imageName))
	fmt.Println("------------------------------------------------------------")

	results, err := image.UnpackAllSubImages(ctx, client, imageName)
	if err != nil {
		logger.Error("Program error", ulog.F("error", err))
		os.Exit(1)
	}

	fmt.Println("------------------------------------------------------------")
	fmt.Println("All sub-images processed successfully. Summary:")
	for kind, sid := range results {
		fmt.Printf("Type: %-15s | SnapshotID: %s\n", kind, sid)
	}
}
