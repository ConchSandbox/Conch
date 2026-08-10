package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseOptionsRequiresExplicitCowSocket(t *testing.T) {
	if _, _, err := parseOptions(nil); err == nil {
		t.Fatal("parseOptions accepted a missing socket path")
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
