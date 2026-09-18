//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/goplus/ixgo"
	_ "github.com/goplus/ixgo/pkg/os"
	"github.com/xgo-dev/sandbox"
)

const interpretedSource = `package main
import "os"
var initialized = 1
var total = 100
type Formula struct { Count int; Text string; OnBuild func(*int) }
func (f *Formula) Setup() {
    step := 2
    f.OnBuild = func(n *int) {
        if initialized != 1 { panic("initialization was lost") }
        b, err := os.ReadFile("/etc/hostname")
        if err != nil { panic(err) }
        f.Text = string(b)
        step++; *n += step; f.Count++; total++
    }
}
`

func inspectIxgo() error {
	i, err := ixgo.NewContext(ixgo.SupportMultipleInterp).LoadInterp("formula.go", interpretedSource)
	if err != nil {
		return err
	}
	defer i.UnsafeRelease()
	if err := i.RunInit(); err != nil {
		return err
	}
	typ, ok := i.GetType("Formula")
	if !ok {
		return fmt.Errorf("missing interpreted Formula")
	}
	class := reflect.New(typ)
	class.Interface().(interface{ Setup() }).Setup()
	holder := &struct {
		Class   reflect.Value
		Type    reflect.Type
		OnBuild func(*int)
	}{
		class.Elem(), typ, class.Elem().FieldByName("OnBuild").Interface().(func(*int)),
	}
	n := 10
	var reads atomic.Int64
	s := sandbox.Sandbox{Inspect: func(call *sandbox.Syscall) {
		if call.Name == "read" {
			reads.Add(1)
		}
	}}
	defer s.Close()
	if err := s.Run(func() { holder.OnBuild(&n) }); err != nil {
		return fmt.Errorf("host-created ixgo closure: %w", err)
	}
	global, _ := i.GetVarAddr("total")
	if n != 13 || holder.Class.FieldByName("Count").Int() != 1 || strings.TrimSpace(holder.Class.FieldByName("Text").String()) == "" || *global.(*int) != 101 || holder.Type != typ || reads.Load() == 0 {
		return fmt.Errorf("ixgo writeback: n=%d class=%v total=%d reads=%d", n, holder.Class, *global.(*int), reads.Load())
	}
	holder.OnBuild(&n)
	if n != 17 || holder.Class.FieldByName("Count").Int() != 2 {
		return fmt.Errorf("host ixgo callback lost state")
	}
	fmt.Println("PASS host-created ixgo method closure, class state, reflection, globals, read syscall and host re-entry")
	return nil
}
