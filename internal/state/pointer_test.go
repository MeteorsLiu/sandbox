package state

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

type pointerNode struct {
	flag bool
	N    int
	Next *pointerNode
}

type pointerNodeView pointerNode

func TestConvertiblePointerRoundTrip(t *testing.T) {
	for _, container := range []string{"none", "struct", "array"} {
		for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
			t.Run(fmt.Sprintf("%s/%v", container, order), func(t *testing.T) {
				var original *pointerNode
				var parent any
				switch container {
				case "none":
					original = new(pointerNode)
				case "struct":
					p := new(struct{ Child pointerNode })
					original, parent = &p.Child, p
				case "array":
					p := new([2]pointerNode)
					original, parent = &p[1], p
				}
				original.flag, original.N, original.Next = true, 10, original
				values := [3]any{original, (*pointerNodeView)(original), parent}
				host := [3]any{values[order[0]], values[order[1]], values[order[2]]}
				var guest [3]any
				var source, destination State
				mem := make([]byte, 1<<20)
				ctx := context.Background()
				for cycle := 0; cycle < 2; cycle++ {
					n, _, err := source.Save(ctx, mem, &host)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := destination.Load(ctx, mem[:n], &guest); err != nil {
						t.Fatal(err)
					}
					var restored [3]any
					for i, index := range order {
						restored[index] = guest[i]
					}
					node, view := restored[0].(*pointerNode), restored[1].(*pointerNodeView)
					if node == original || (*pointerNode)(view) != node || node.Next != node || !node.flag || node.N != 10+cycle {
						t.Fatal("pointer conversion lost isolation, identity, cycle or value")
					}
					switch container {
					case "struct":
						if &restored[2].(*struct{ Child pointerNode }).Child != node {
							t.Fatal("converted field pointer lost its parent")
						}
					case "array":
						if &restored[2].(*[2]pointerNode)[1] != node {
							t.Fatal("converted element pointer lost its array")
						}
					}
					view.N++
					if node.N != 11+cycle || original.N != 10+cycle {
						t.Fatal("converted pointer copied storage or changed the host")
					}
					n, _, err = destination.Save(ctx, mem, &guest)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := source.Load(ctx, mem[:n], &host); err != nil {
						t.Fatal(err)
					}
					for i, index := range order {
						if host[i] != values[index] {
							t.Fatal("return replaced an original pointer")
						}
					}
					if original.N != 11+cycle || original.Next != original {
						t.Fatal("return lost the mutation or original cycle")
					}
				}
			})
		}
	}
}

type graphNodeView graphNode

type pointerLoadOrder struct {
	Original *graphNode
	View     *graphNodeView
}

func (*pointerLoadOrder) StateTypeName() string { return "state.test.pointerLoadOrder" }
func (*pointerLoadOrder) StateFields() []string { return []string{"Original", "View"} }
func (p *pointerLoadOrder) StateSave(s Sink) {
	s.Save(0, &p.Original)
	s.Save(1, &p.View)
}
func (p *pointerLoadOrder) StateLoad(_ context.Context, s Source) {
	// Decode the converted pointer first; graphNode's custom loader must
	// still run, even though graphNodeView has no StateLoad method.
	s.Load(1, &p.View)
	s.Load(0, &p.Original)
}

func init() { Register((*pointerLoadOrder)(nil)) }

func TestConvertiblePointerLoadOrder(t *testing.T) {
	n := &graphNode{Value: 42}
	n.Next = n
	src := pointerLoadOrder{Original: n, View: (*graphNodeView)(n)}
	var dst pointerLoadOrder
	roundtrip(t, &src, &dst)
	if dst.Original == n || dst.Original != (*graphNode)(dst.View) || dst.Original.Next != dst.Original || dst.Original.Value != 42 || !dst.Original.loaded {
		t.Fatal("decode order changed object identity or bypassed the original type's loader")
	}
}

func TestIncompatiblePointerViews(t *testing.T) {
	n := int64(42)
	other := reflect.NewAt(reflect.TypeFor[float64](), reflect.ValueOf(&n).UnsafePointer()).Interface()
	root := [2]any{&n, other}
	if _, _, err := Save(context.Background(), make([]byte, 1<<20), &root); err == nil {
		t.Fatal("incompatible pointer types sharing an address were accepted")
	}
}
