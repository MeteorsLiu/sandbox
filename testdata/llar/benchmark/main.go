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
)

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
	CallbackNS int64  `json:"callback_duration_ns"`
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
	rootfs := flag.String("rootfs", "/", "Sentry guest root filesystem; does not change the host root")
	count := flag.Int("count", 1, "number of complete builds")
	concurrency := flag.Int("concurrency", 1, "simultaneous builds")
	flag.Parse()
	if *count < 1 || *concurrency < 1 || (*backend != "direct" && *backend != "sandbox") {
		return fmt.Errorf("invalid backend, count or concurrency")
	}
	if err := os.MkdirAll(*work, 0755); err != nil {
		return err
	}
	if *backend == "sandbox" {
		// The native host keeps its UID; the guest writes as UID 1000.
		if err := os.Chmod(*work, 0777); err != nil {
			return err
		}
	}
	jobs := make([]job, *count)
	formulaFS := os.DirFS(filepath.Join(*input, "formula")).(fs.ReadFileFS)
	builds := make([]func(*formulapkg.Context), min(*concurrency, *count))
	for i := range builds {
		loaded, err := formula.LoadFS(formulaFS, "v1.3.1/zlib_llar.gox")
		if err != nil {
			return fmt.Errorf("load Formula: %w", err)
		}
		if loaded.ModPath != "madler/zlib" || loaded.OnBuild == nil {
			return fmt.Errorf("unexpected Formula %q", loaded.ModPath)
		}
		builds[i] = loaded.OnBuild
	}
	for i := range jobs {
		dir := filepath.Join(*work, fmt.Sprintf("job-%d", i))
		source, out := filepath.Join(dir, "source"), filepath.Join(dir, "install")
		if err := os.CopyFS(source, os.DirFS(filepath.Join(*input, "source"))); err != nil {
			return err
		}
		if err := os.MkdirAll(out, 0755); err != nil {
			return err
		}
		jobs[i].out = out
		if *backend == "sandbox" {
			if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				return os.Chmod(path, info.Mode().Perm()|0222)
			}); err != nil {
				return err
			}
			// Only the guest sees this workspace at /work, as in Docker and the VM.
			source = filepath.Join("/work", filepath.Base(dir), "source")
			out = filepath.Join("/work", filepath.Base(dir), "install")
		}
		jobs[i].ctx = formulapkg.NewContext(&formulapkg.Project{SourceFS: formulaFS}, source, out, "linux/"+runtime.GOARCH, nil)
	}
	// Each interpreter is prepared before concurrent transfers: constructing
	// reflectx types while Save/Load enumerates their caches is unsupported.
	s := sandbox.Sandbox{
		Library: *library,
		Mounts: []sandbox.Mount{
			{Type: "bind", Source: *rootfs, Target: "/", Options: []string{"ro"}},
			{Type: "bind", Source: *work, Target: "/work", Options: []string{"rw"}},
			{Type: "tmpfs", Target: "/tmp", Options: []string{"mode=1777"}},
			{Type: "proc", Target: "/proc"},
		},
		Env: os.Environ(),
	}
	defer s.Close()
	emit(map[string]any{"event": "ready", "backend": *backend, "count": *count, "concurrency": *concurrency, "prepare_ns": time.Since(started).Nanoseconds(), "gomaxprocs": runtime.GOMAXPROCS(0), "go_version": runtime.Version(), "uid": os.Geteuid(), "makeflags": os.Getenv("MAKEFLAGS")})
	queue := make(chan int)
	results := make(chan result, *count)
	var workers sync.WaitGroup
	begin := time.Now()
	for worker := range builds {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range queue {
				t0 := time.Now()
				emit(map[string]any{"event": "run_start", "id": id})
				r := result{Event: "result", ID: id}
				j := jobs[id]
				j.build = builds[worker]
				var err error
				r.CallbackNS, err = execute(j, *backend, &s, id)
				r.DurationNS = time.Since(t0).Nanoseconds()
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

func execute(j job, backend string, s *sandbox.Sandbox, id int) (callbackNS int64, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("Formula panic: %v", p)
		}
	}()
	build, ctx := j.build, j.ctx
	start := fmt.Sprintf("BENCH:{\"event\":\"callback_start\",\"id\":%d}\n", id)
	end := fmt.Sprintf("BENCH:{\"event\":\"callback_end\",\"id\":%d}\n", id)
	if backend == "sandbox" {
		err = s.Run(func() {
			if _, err := os.Stdout.WriteString(start); err != nil {
				panic(err)
			}
			begin := time.Now()
			build(ctx)
			callbackNS = time.Since(begin).Nanoseconds()
			if _, err := os.Stdout.WriteString(end); err != nil {
				panic(err)
			}
		})
	} else {
		emit(map[string]any{"event": "callback_start", "id": id})
		begin := time.Now()
		build(ctx)
		callbackNS = time.Since(begin).Nanoseconds()
		emit(map[string]any{"event": "callback_end", "id": id})
	}
	if err != nil {
		return callbackNS, err
	}
	if len(ctx.Errs) != 0 {
		return callbackNS, ctx.Errs.ToError()
	}
	if !strings.Contains(ctx.Out.Metadata(), "-lz") {
		return callbackNS, fmt.Errorf("missing zlib metadata: %q", ctx.Out.Metadata())
	}
	for _, name := range []string{"lib/libz.a", "include/zlib.h", "lib/pkgconfig/zlib.pc", "licenses/LICENSE"} {
		info, err := os.Stat(filepath.Join(j.out, name))
		if err != nil || info.Size() == 0 {
			return callbackNS, fmt.Errorf("missing/empty artifact %s: %v", name, err)
		}
	}
	return callbackNS, nil
}
