package state

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStandardStreams(t *testing.T) {
	src := []*os.File{os.Stdin, os.Stdout, os.Stderr, os.Stdout, nil}
	var dst []*os.File
	roundtrip(t, &src, &dst)
	for i := range src {
		if dst[i] != src[i] {
			t.Fatalf("stream %d was copied instead of rebound", i)
		}
	}
}

func TestProcessResourcesRejected(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "resource")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for _, input := range []any{f, &os.Process{}, ctx, timer} {
		if _, _, err := Save(context.Background(), make([]byte, 1<<20), &input); err == nil || !strings.Contains(err.Error(), "process-local state") {
			t.Fatalf("%T: %v", input, err)
		}
	}
}
