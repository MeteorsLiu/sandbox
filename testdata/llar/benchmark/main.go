//go:build linux && (amd64 || arm64) && cgo

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	formulapkg "github.com/goplus/llar/formula"
	"github.com/goplus/llar/internal/formula"
	"github.com/xgo-dev/sandbox"
	"golang.org/x/sys/unix"
)

const entryMarker = 0x53424d31

var outputMu sync.Mutex

func emit(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	outputMu.Lock()
	fmt.Printf("BENCH:%s\n", data)
	outputMu.Unlock()
}

type job struct {
	build func(*formulapkg.Context)
	ctx   *formulapkg.Context
	out   string
}

type result struct {
	Event      string `json:"event"`
	ID         int    `json:"id"`
	DurationNS int64  `json:"duration_ns"`
	EntryNS    int64  `json:"entry_ns,omitempty"`
	Metadata   string `json:"metadata,omitempty"`
	Error      string `json:"error,omitempty"`
}

func main() {
	started := time.Now()
	emit(map[string]any{"event": "main", "unix_ns": started.UnixNano()})
	if err := run(started); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(started time.Time) error {
	backend := flag.String("backend", "direct", "direct or sandbox")
	input := flag.String("input", "/opt/benchmark/input", "prepared Formula and source directory")
	work := flag.String("work", "/work", "empty writable build directory")
	library := flag.String("library", "/opt/benchmark/sentrylib.so", "matching Sentry library")
	count := flag.Int("count", 1, "number of complete builds")
	concurrency := flag.Int("concurrency", 1, "simultaneous builds")
	flag.Parse()
	if *count < 1 || *concurrency < 1 || (*backend != "direct" && *backend != "sandbox") {
		return fmt.Errorf("invalid backend, count or concurrency")
	}
	if err := os.MkdirAll(*work, 0755); err != nil {
		return err
	}
	jobs := make([]job, *count)
	for i := range jobs {
		dir := filepath.Join(*work, fmt.Sprintf("job-%d", i))
		source, out := filepath.Join(dir, "source"), filepath.Join(dir, "install")
		if err := os.CopyFS(source, os.DirFS(filepath.Join(*input, "source"))); err != nil {
			return err
		}
		if err := os.MkdirAll(out, 0755); err != nil {
			return err
		}
		formulaFS := os.DirFS(filepath.Join(*input, "formula")).(fs.ReadFileFS)
		loaded, err := formula.LoadFS(formulaFS, "v1.3.1/zlib_llar.gox")
		if err != nil {
			return fmt.Errorf("load Formula: %w", err)
		}
		if loaded.ModPath != "madler/zlib" || loaded.OnBuild == nil {
			return fmt.Errorf("unexpected Formula %q", loaded.ModPath)
		}
		jobs[i] = job{
			build: loaded.OnBuild,
			ctx:   formulapkg.NewContext(&formulapkg.Project{SourceFS: formulaFS}, source, out, "linux/"+runtime.GOARCH, nil),
			out:   out,
		}
	}
	// Each interpreter is prepared before concurrent transfers: constructing
	// reflectx types while Save/Load enumerates their caches is unsupported.
	s := sandbox.Sandbox{
		Library: *library,
		Mounts: []sandbox.Mount{
			{Type: "bind", Source: "/", Target: "/", Options: []string{"ro"}},
			{Type: "bind", Source: *work, Target: *work, Options: []string{"rw"}},
			{Type: "tmpfs", Target: "/tmp", Options: []string{"mode=1777"}},
			{Type: "proc", Target: "/proc"},
		},
		Env: os.Environ(),
	}
	defer s.Close()
	entries := make(chan time.Time, 1)
	if *backend == "sandbox" && *concurrency == 1 {
		// The benchmark build inserts this getpid at guestEntry's first line.
		// Only serial runs report per-call entry latency; no cross-call guessing.
		s.Inspect = func(call *sandbox.Syscall) {
			if call.Number == unix.SYS_GETPID && call.Args[0] == entryMarker {
				entries <- time.Now()
				emit(map[string]any{"event": "guest_entry"})
			}
		}
	}
	emit(map[string]any{"event": "ready", "backend": *backend, "count": *count, "concurrency": *concurrency, "prepare_ns": time.Since(started).Nanoseconds()})
	queue := make(chan int)
	results := make(chan result, *count)
	var workers sync.WaitGroup
	begin := time.Now()
	for range min(*concurrency, *count) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range queue {
				t0 := time.Now()
				r := result{Event: "result", ID: id}
				err := execute(jobs[id], *backend, &s)
				r.DurationNS = time.Since(t0).Nanoseconds()
				select {
				case entry := <-entries:
					r.EntryNS = entry.Sub(t0).Nanoseconds()
				default:
				}
				if err != nil {
					r.Error = err.Error()
				} else {
					r.Metadata = jobs[id].ctx.Out.Metadata()
				}
				emit(r)
				results <- r
			}
		}()
	}
	for i := range jobs {
		queue <- i
	}
	close(queue)
	workers.Wait()
	close(results)
	failed := 0
	for r := range results {
		if r.Error != "" {
			failed++
		}
	}
	emit(map[string]any{"event": "summary", "backend": *backend, "count": *count, "concurrency": *concurrency, "failed": failed, "wall_ns": time.Since(begin).Nanoseconds(), "total_ns": time.Since(started).Nanoseconds()})
	if failed != 0 {
		return fmt.Errorf("%d/%d builds failed", failed, *count)
	}
	return nil
}

func execute(j job, backend string, s *sandbox.Sandbox) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("Formula panic: %v", p)
		}
	}()
	build, ctx := j.build, j.ctx
	if backend == "sandbox" {
		err = s.Run(func() { build(ctx) })
	} else {
		build(ctx)
	}
	if err != nil {
		return err
	}
	if len(ctx.Errs) != 0 {
		return ctx.Errs.ToError()
	}
	if !strings.Contains(ctx.Out.Metadata(), "-lz") {
		return fmt.Errorf("missing zlib metadata: %q", ctx.Out.Metadata())
	}
	for _, name := range []string{"lib/libz.a", "include/zlib.h", "lib/pkgconfig/zlib.pc", "licenses/LICENSE"} {
		info, err := os.Stat(filepath.Join(j.out, name))
		if err != nil || info.Size() == 0 {
			return fmt.Errorf("missing/empty artifact %s: %v", name, err)
		}
	}
	return nil
}
