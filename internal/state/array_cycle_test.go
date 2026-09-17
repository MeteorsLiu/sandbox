package state

import (
	"context"
	"strings"
	"testing"
)

type arrayCycleNode struct {
	Value int
	Next  [2]*arrayCycleNode
}

func TestInlineArrayCycles(t *testing.T) {
	a, b, c := &arrayCycleNode{Value: 10}, &arrayCycleNode{Value: 20}, &arrayCycleNode{Value: 30}
	a.Next = [2]*arrayCycleNode{b, a}
	b.Next = [2]*arrayCycleNode{a, c}
	c.Next = [2]*arrayCycleNode{b, a}
	src := [3]*arrayCycleNode{a, b, c}
	var dst [3]*arrayCycleNode
	roundtrip(t, &src, &dst)
	for i, node := range dst {
		if node == src[i] || node.Value != src[i].Value {
			t.Fatal("restored node lost its value or shares source storage")
		}
	}
	if dst[0].Next != [2]*arrayCycleNode{dst[1], dst[0]} ||
		dst[1].Next != [2]*arrayCycleNode{dst[0], dst[2]} ||
		dst[2].Next != [2]*arrayCycleNode{dst[1], dst[0]} {
		t.Fatal("array cycles or shared references changed")
	}
}

type arrayLoadWaiter struct {
	Nodes      [2][2]*graphNode
	UseValue   bool
	ValueCalls int
	ValueReady bool
	Loaded     bool
}

func (*arrayLoadWaiter) StateTypeName() string { return "state.test.arrayLoadWaiter" }
func (*arrayLoadWaiter) StateFields() []string { return []string{"Nodes", "UseValue"} }
func (v *arrayLoadWaiter) StateSave(s Sink) {
	s.Save(0, &v.Nodes)
	s.Save(1, &v.UseValue)
}
func (v *arrayLoadWaiter) StateLoad(_ context.Context, s Source) {
	s.Load(1, &v.UseValue)
	if v.UseValue {
		s.LoadValue(0, &v.Nodes, func(any) {
			v.ValueCalls++
			v.ValueReady = v.Nodes[0][0].loaded && v.Nodes[1][0].loaded
		})
	} else {
		s.LoadWait(0, &v.Nodes)
	}
	s.AfterLoad(func() {
		v.Loaded = v.Nodes[0][0].loaded && v.Nodes[1][0].loaded
		if v.UseValue {
			v.Loaded = v.Loaded && v.ValueReady && v.ValueCalls == 1
		}
	})
}

type arrayExplicitCycle struct {
	Next [1]*arrayExplicitCycle
}

func (*arrayExplicitCycle) StateTypeName() string                   { return "state.test.arrayExplicitCycle" }
func (*arrayExplicitCycle) StateFields() []string                   { return []string{"Next"} }
func (v *arrayExplicitCycle) StateSave(s Sink)                      { s.Save(0, &v.Next) }
func (v *arrayExplicitCycle) StateLoad(_ context.Context, s Source) { s.LoadWait(0, &v.Next) }

func init() {
	Register((*arrayLoadWaiter)(nil))
	Register((*arrayExplicitCycle)(nil))
}

func TestInlineArrayWaitCallbacks(t *testing.T) {
	for _, useValue := range []bool{false, true} {
		a, b := &graphNode{Value: 10}, &graphNode{Value: 20}
		a.Next, b.Next = b, a
		src := arrayLoadWaiter{
			Nodes:    [2][2]*graphNode{{a, a}, {b, a}},
			UseValue: useValue,
		}
		var dst arrayLoadWaiter
		roundtrip(t, &src, &dst)
		if !dst.Loaded || dst.Nodes[0][0] != dst.Nodes[1][1] || dst.Nodes[0][0].Next != dst.Nodes[1][0] || dst.Nodes[1][0].Next != dst.Nodes[0][0] {
			t.Fatalf("UseValue=%t: callback order or array aliases changed: calls=%d ready=%t loaded=%t", useValue, dst.ValueCalls, dst.ValueReady, dst.Loaded)
		}
	}
}

func TestInlineArrayExplicitWaitCycle(t *testing.T) {
	a, b := new(arrayExplicitCycle), new(arrayExplicitCycle)
	a.Next[0], b.Next[0] = b, a
	mem := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), mem, &a)
	if err != nil {
		t.Fatal(err)
	}
	var dst *arrayExplicitCycle
	if _, err := Load(context.Background(), mem[:n], &dst); err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("explicit LoadWait cycle must remain an error, got %v", err)
	}
}
