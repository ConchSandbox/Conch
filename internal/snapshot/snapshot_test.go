package snapshot

import (
	"context"
	"testing"
	"time"
)

func TestPrepare(t *testing.T) {
	workDir := "/tmp/snapshot"
	err := NewServer(workDir)
	if err != nil {
		t.Fatalf("init server with: %s, get err: %v", workDir, err)
	}
	ns := "default"
	key := "hello"
	parent := "sha256:9864188ae7e73d7d0e5e4f52441721380a1564c262a0fbf5795a594c281bf737"
	conf, err := Prepare(context.Background(), ns, key, parent)
	if err != nil {
		t.Fatalf("get error: %v\n", err)
	}
	t.Logf("prepare snapshot result: %v\n", conf)
	newKey := "hello-commit"
	if err := Commit(context.Background(), ns, newKey, key); err != nil {
		t.Fatalf("commit snapshot failed: %v\n", err)
	}
	t.Logf("finish commit snapshot: %s\n", newKey)

	time.Sleep(time.Second * 1)
	t.Logf("run remove snapshot: %s\n", newKey)
	if err := Remove(context.Background(), ns, newKey); err != nil {
		t.Fatalf("remove snapshot failed: %v\n", err)
	}
}
