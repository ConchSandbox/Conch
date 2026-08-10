package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/cow"
)

func TestParseOptionsUsesCowSocketDefault(t *testing.T) {
	options, help, err := parseOptions(nil)
	if err != nil || help {
		t.Fatalf("parseOptions() help=%v err=%v", help, err)
	}
	if options.socketPath != cow.DefaultSocketPath {
		t.Fatalf("socket = %q, want %q", options.socketPath, cow.DefaultSocketPath)
	}
	if _, _, err := parseOptions([]string{"extra"}); err == nil {
		t.Fatal("parseOptions accepted a positional argument")
	}
	if _, help, err := parseOptions([]string{"--help"}); err != nil || !help {
		t.Fatalf("parseOptions(--help) help=%v err=%v", help, err)
	}
}

func TestRunServesUntilContextCancellation(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cow.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"--socket", socketPath}) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("conch-cow did not create its socket")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("conch-cow did not stop after cancellation")
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after run returned: %v", err)
	}
}
