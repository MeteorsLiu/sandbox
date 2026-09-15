package state

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
)

type graphNode struct {
	Value  int64
	Next   *graphNode
	loaded bool
}

func (*graphNode) StateTypeName() string { return "state.test.graphNode" }
func (*graphNode) StateFields() []string { return []string{"Value", "Next"} }
func (n *graphNode) StateSave(s Sink) {
	s.Save(0, &n.Value)
	s.Save(1, &n.Next)
}
func (n *graphNode) StateLoad(_ context.Context, s Source) {
	s.Load(0, &n.Value)
	s.Load(1, &n.Next)
	s.AfterLoad(func() { n.loaded = true })
}

func init() { Register((*graphNode)(nil)) }

func roundtrip(t *testing.T, src, dst any) {
	t.Helper()
	mem := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), mem, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), mem[:n], dst); err != nil {
		t.Fatal(err)
	}
}

func TestGraphInteriorPointer(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		n := &graphNode{Value: 42}
		n.Next = n
		root := [2]any{&n.Value, n}
		if parentFirst {
			root[0], root[1] = root[1], root[0]
		}
		var got [2]any
		roundtrip(t, &root, &got)
		i := 0
		if parentFirst {
			i = 1
		}
		field, parent := got[i].(*int64), got[1-i].(*graphNode)
		if field != &parent.Value || parent.Next != parent || parent == n || !parent.loaded {
			t.Fatalf("parentFirst=%v: alias, cycle or load callback was lost", parentFirst)
		}
		*field = 99
		if parent.Value != 99 || n.Value != 42 {
			t.Fatal("decoded graph does not own its storage")
		}
	}
}

func TestGraphArrayInteriorPointer(t *testing.T) {
	n := &[3]int{10, 20, 30}
	root := [2]any{&n[1], n}
	var got [2]any
	roundtrip(t, &root, &got)
	if got[0].(*int) != &got[1].(*[3]int)[1] {
		t.Fatal("array element alias lost")
	}
}

func TestGraphMapAndSliceAliases(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	b := []int{1, 2, 3}
	root := []any{m, m, b[:2], b}
	var got []any
	roundtrip(t, &root, &got)
	m1, m2 := got[0].(map[string]any), got[1].(map[string]any)
	m1["new"] = 42
	if m2["new"] != 42 || m1["self"].(map[string]any)["new"] != 42 {
		t.Fatal("map alias or cycle lost")
	}
	got[2].([]int)[0] = 9
	if got[3].([]int)[0] != 9 || len(got[2].([]int)) != 2 || cap(got[2].([]int)) != 3 {
		t.Fatal("slice alias or length/capacity lost")
	}
}

func TestGraphValues(t *testing.T) {
	values := []any{
		false, true, int(-42), int8(-8), int16(-16), int32(-32), int64(-64),
		uint(42), uint8(8), uint16(16), uint32(32), uint64(64), uintptr(123),
		float32(1.5), float64(-2.5), complex64(complex(1, 2)), complex128(complex(3, 4)),
		"", "hello\x00world", [3]int{1, 2, 3}, []int(nil), []int{}, map[string]int(nil), map[string]int{},
		(*int)(nil), struct{}{},
	}
	for _, value := range values {
		t.Run(reflect.TypeOf(value).String(), func(t *testing.T) {
			src := reflect.New(reflect.TypeOf(value))
			src.Elem().Set(reflect.ValueOf(value))
			dst := reflect.New(src.Elem().Type())
			roundtrip(t, src.Interface(), dst.Interface())
			if !reflect.DeepEqual(src.Elem().Interface(), dst.Elem().Interface()) {
				t.Fatalf("got %#v, want %#v", dst.Elem().Interface(), value)
			}
		})
	}
}

func TestGraphMemoryErrors(t *testing.T) {
	n := 42
	if _, _, err := Save(context.Background(), nil, &n); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("short buffer: %v", err)
	}
	mem := make([]byte, 1024)
	size, _, err := Save(context.Background(), mem, &n)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < size; i++ {
		var dst int
		if _, err := Load(context.Background(), mem[:i], &dst); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("prefix %d/%d: %v", i, size, err)
		}
	}
}
