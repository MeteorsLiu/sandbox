//go:build linux && (amd64 || arm64) && cgo

// Test selection and skips are from github.com/goplus/ixgo v1.1.6,
// cmd/ixgotest/main.go (Apache-2.0). Execution is adapted to sandbox.Run.

package goroot_test

import (
	"path/filepath"
	"runtime"
	"strconv"
)

var (
	gorootTestSkips = make(map[string]string)
)

func init() {
	if runtime.GOARCH == "386" {
		gorootTestSkips["printbig.go"] = "load failed"
		gorootTestSkips["peano.go"] = "stack overflow"
	}
	gorootTestSkips["closure.go"] = "runtime.ReadMemStats"
	gorootTestSkips["divmod.go"] = "slow, 1m18s"
	gorootTestSkips["copy.go"] = "slow, 13s"
	gorootTestSkips["finprofiled.go"] = "slow, 21s"
	gorootTestSkips["gcgort.go"] = "slow, 2s"
	gorootTestSkips["nilptr.go"] = "skip drawin"
	gorootTestSkips["heapsampling.go"] = "runtime.MemProfileRecord"
	gorootTestSkips["makeslice.go"] = "TODO, panic info, allocation size out of range"
	// gorootTestSkips["stackobj.go"] = "skip gc"
	// gorootTestSkips["stackobj3.go"] = "skip gc"
	gorootTestSkips["nilptr_aix.go"] = "skip"
	// gorootTestSkips["init1.go"] = "skip gc"
	gorootTestSkips["ken/divconst.go"] = "slow, 3.5s"
	gorootTestSkips["ken/modconst.go"] = "slow, 3.3s"
	gorootTestSkips["fixedbugs/issue24491b.go"] = "timeout"
	gorootTestSkips["fixedbugs/issue16249.go"] = "slow, 4.5s"
	gorootTestSkips["fixedbugs/issue13169.go"] = "slow, 5.9s"
	gorootTestSkips["fixedbugs/issue11656.go"] = "ignore"
	// gorootTestSkips["fixedbugs/issue15281.go"] = "runtime.ReadMemStats"
	gorootTestSkips["fixedbugs/issue18149.go"] = "runtime.Caller macos //line not support c:/foo/bar.go:987"
	gorootTestSkips["fixedbugs/issue22662.go"] = "runtime.Caller got $goroot/test/fixedbugs/foo.go:1; want foo.go:1"
	// gorootTestSkips["fixedbugs/issue27518b.go"] = "BUG, runtime.SetFinalizer"
	// gorootTestSkips["fixedbugs/issue32477.go"] = "BUG, runtime.SetFinalizer"
	gorootTestSkips["fixedbugs/issue41239.go"] = "BUG, reflect.Append: different capacity on append"
	// gorootTestSkips["fixedbugs/issue32477.go"] = "BUG, runtime.SetFinalizer"
	gorootTestSkips["fixedbugs/issue45175.go"] = "BUG, ssa.Phi call order"
	gorootTestSkips["fixedbugs/issue4618.go"] = "testing.AllocsPerRun"
	gorootTestSkips["fixedbugs/issue4667.go"] = "testing.AllocsPerRun"
	gorootTestSkips["fixedbugs/issue8606b.go"] = "BUG, optimization check"
	gorootTestSkips["fixedbugs/issue30116u.go"] = "BUG, slice bound check"
	gorootTestSkips["chan/select5.go"] = "bug, select case expr call order"

	// Sandbox-specific: ixgo Goexit compares the current goroutine ID with the
	// interpreter's host mainid. Exclude these cases until IDs are rebound.
	gorootTestSkips["fixedbugs/issue5963.go"] = "ixgo Goexit depends on a process-local goroutine ID"
	gorootTestSkips["fixedbugs/issue8158.go"] = "ixgo Goexit depends on a process-local goroutine ID"
	gorootTestSkips["fixedbugs/issue11256.go"] = "ixgo Goexit depends on a process-local goroutine ID"

	// fixedbugs/issue7740.go
	// const ulp = (1.0 + (2.0 / 3.0)) - (5.0 / 3.0)
	// Go 1.14 1.15 1.16 ulp = 1.4916681462400413e-154
	// Go 1.17 1.18 ulp = 0

	// go1.24.3 => 24
	ver, err := strconv.Atoi(runtime.Version()[4:6])
	if err != nil {
		panic("version error")
	}
	switch {
	case ver >= 17:
		// gorootTestSkips["fixedbugs/issue45045.go"] = "runtime.SetFinalizer"
		// gorootTestSkips["fixedbugs/issue46725.go"] = "runtime.SetFinalizer"
		gorootTestSkips["abi/fibish.go"] = "slow, 34s"
		gorootTestSkips["abi/fibish_closure.go"] = "slow, 35s"
		gorootTestSkips["abi/uglyfib.go"] = "5m48s"
		// gorootTestSkips["fixedbugs/issue23017.go"] = "BUG" //fixed https://github.com/golang/go/issues/55086

		gorootTestSkips["typeparam/chans.go"] = "runtime.SetFinalizer, maybe broken for go1.18 on linux workflows"
		// gorootTestSkips["typeparam/issue376214.go"] = "build SSA package error: variadic parameter must be of unnamed slice type"
		if ver != 20 {
			gorootTestSkips["typeparam/nested.go"] = "skip, run pass but output same as go1.20"
		}
		//go1.20
		gorootTestSkips["fixedbugs/bug514.go"] = "skip cgo"
		gorootTestSkips["fixedbugs/issue40954.go"] = "skip cgo"
		gorootTestSkips["fixedbugs/issue42032.go"] = "skip cgo"
		gorootTestSkips["fixedbugs/issue42076.go"] = "skip cgo"
		gorootTestSkips["fixedbugs/issue46903.go"] = "skip cgo"
		gorootTestSkips["fixedbugs/issue51733.go"] = "skip cgo"
		//go1.21
		gorootTestSkips["fixedbugs/issue19658.go"] = "skip command"
		// gorootTestSkips["fixedbugs/issue57823.go"] = "GC"
		if ver == 18 {
			gorootTestSkips["typeparam/cons.go"] = "skip golang.org/x/tools v0.7.0 on go1.18"
			gorootTestSkips["typeparam/list2.go"] = "skip golang.org/x/tools v0.7.0 on go1.18"
		}
		if ver >= 22 {
			gorootTestSkips["fixedbugs/bug369.go"] = "skip command"
			gorootTestSkips["fixedbugs/issue10607.go"] = "skip command"
			gorootTestSkips["fixedbugs/issue21317.go"] = "skip command"
			gorootTestSkips["fixedbugs/issue38093.go"] = "skip js"
			gorootTestSkips["fixedbugs/issue64565.go"] = "skip command"
			gorootTestSkips["fixedbugs/issue9355.go"] = "skip command"
			gorootTestSkips["fixedbugs/issue69110.go"] = "skip runtime link"
			gorootTestSkips["linkmain_run.go"] = "skip link"
			gorootTestSkips["linkobj.go"] = "skip link"
			gorootTestSkips["linkx_run.go"] = "skip link"
			gorootTestSkips["chanlinear.go"] = "skip -gc-exp"
		}
		if ver >= 25 {
			gorootTestSkips["fixedbugs/issue72844.go"] = "BUG, range for nil *op[N]"
			gorootTestSkips["fixedbugs/issue73476.go"] = "BUG, range for nil *op[N]"
		}
		if ver >= 26 {
			gorootTestSkips["range4.go"] = "BUG, range"
			gorootTestSkips["rangegen.go"] = "BUG, range"
		}
		if ver >= 27 {
			gorootTestSkips["genmeth1.go"] = "buld ssa package error: assertion failed"
		}
	case ver == 16:
		gorootTestSkips["fixedbugs/issue7740.go"] = "BUG, const float"
	case ver == 15:
		gorootTestSkips["fixedbugs/issue15039.go"] = "BUG, uint64 -> string"
		gorootTestSkips["fixedbugs/issue9355.go"] = "TODO, chdir"
		gorootTestSkips["fixedbugs/issue7740.go"] = "BUG, const float"
	case ver == 14:
		gorootTestSkips["fixedbugs/issue9355.go"] = "TODO, chdir"
		gorootTestSkips["fixedbugs/issue7740.go"] = "BUG, const float"
	}

	if runtime.GOOS == "windows" {
		gorootTestSkips["env.go"] = "skip GOARCH"
		gorootTestSkips["fixedbugs/issue15002.go"] = "skip windows"
		gorootTestSkips["fixedbugs/issue5493.go"] = "skip windows"
		gorootTestSkips["fixedbugs/issue5963.go"] = "skip windows"
		gorootTestSkips["fixedbugs/issue79874.go"] = "skip windows"
		if ver >= 22 {
			gorootTestSkips["recover4.go"] = "skip windows"
			gorootTestSkips["sigchld.go"] = "skip windows"
		}

		skips := make(map[string]string)
		for k, v := range gorootTestSkips {
			skips[filepath.FromSlash(k)] = v
		}
		gorootTestSkips = skips
	} else if runtime.GOOS == "darwin" {
		gorootTestSkips["locklinear.go"] = "skip github"
		gorootTestSkips["env.go"] = "skip github"
	}
}
