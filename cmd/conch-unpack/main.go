package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"

	"github.com/openeuler/Conch/internal/image"
)

const defaultContainerdAddress = "/run/containerd/containerd.sock"

func main() {
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
		log.Fatalf("failed to connect to containerd: %v", err)
	}
	defer client.Close()

	fmt.Printf("Starting analysis for main image: %s\n", imageName)
	fmt.Println("------------------------------------------------------------")

	results, err := image.UnpackAllSubImages(ctx, client, imageName)
	if err != nil {
		fmt.Printf("Program error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("------------------------------------------------------------")
	fmt.Println("All sub-images processed successfully. Summary:")
	for kind, sid := range results {
		fmt.Printf("Type: %-15s | SnapshotID: %s\n", kind, sid)
	}
}
