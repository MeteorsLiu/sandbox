package state

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

func TestChannelValues(t *testing.T) {
	for _, capacity := range []int{0, 1, 4} {
		for _, closed := range []bool{false, true} {
			src := make(chan int, capacity)
			for i := 0; i < capacity; i++ {
				src <- 10 + i
			}
			if capacity > 0 {
				<-src
				src <- 10 + capacity
			}
			if closed {
				close(src)
			}
			var dst chan int
			roundtrip(t, &src, &dst)
			if dst == nil || dst == src || len(dst) != capacity || cap(dst) != capacity || len(src) != capacity {
				t.Fatalf("capacity=%d closed=%v: channel identity or size changed", capacity, closed)
			}
			for i := 0; i < capacity; i++ {
				got, ok := <-dst
				if !ok || got != 11+i || <-src != got {
					t.Fatal("queue order changed or source was consumed")
				}
			}
			select {
			case _, ok := <-dst:
				if !closed || ok {
					t.Fatal("restored channel has wrong closed state")
				}
			default:
				if closed {
					t.Fatal("restored channel is not closed")
				}
			}
		}
	}
	var src, dst chan int
	roundtrip(t, &src, &dst)
	if dst != nil {
		t.Fatal("nil channel became non-nil")
	}
}

func TestChannelAliases(t *testing.T) {
	type receive <-chan int
	type send chan<- int
	type named chan int
	c := make(chan int, 3)
	c <- 42
	src := []any{receive(c), send(c), named(c), c, reflect.ValueOf(c), map[chan int]string{c: "queue"}}
	var dst []any
	roundtrip(t, &src, &dst)
	got := dst[3].(chan int)
	if got == c || (<-chan int)(got) != dst[0].(receive) || (chan<- int)(got) != dst[1].(send) || got != dst[2].(named) || got != dst[4].(reflect.Value).Interface().(chan int) || dst[5].(map[chan int]string)[got] != "queue" {
		t.Fatal("named, directional, reflected or map-key aliases changed")
	}
	dst[1].(send) <- 43
	if <-dst[0].(receive) != 42 || <-got != 43 || len(c) != 1 {
		t.Fatal("restored aliases do not share an independent queue")
	}
}

func TestChannelPointersAndCycles(t *testing.T) {
	n := &graphNode{Value: 42}
	n.Next = n
	c := make(chan any, 3)
	other := make(chan any, 1)
	c <- n
	c <- c
	c <- other
	other <- c
	src := []any{c, n}
	var dst []any
	roundtrip(t, &src, &dst)
	runtime.GC()
	got, node := dst[0].(chan any), dst[1].(*graphNode)
	if <-got != node || node == n || node.Next != node || !node.loaded || (<-got).(chan any) != got {
		t.Fatal("channel element pointers or self-reference changed")
	}
	if (<-(<-got).(chan any)).(chan any) != got {
		t.Fatal("mutual channel cycle changed")
	}
	if len(c) != 3 || len(other) != 1 {
		t.Fatal("snapshot consumed the source")
	}
}

func TestChannelZeroSizedElements(t *testing.T) {
	src := make(chan struct{}, 3)
	src <- struct{}{}
	src <- struct{}{}
	close(src)
	var dst chan struct{}
	roundtrip(t, &src, &dst)
	count := 0
	for range dst {
		count++
	}
	if count != 2 || cap(dst) != 3 || len(src) != 2 {
		t.Fatal("zero-sized channel elements changed")
	}
}

type channelWaiter struct {
	Queue  chan graphNode
	Loaded bool
}

func (*channelWaiter) StateTypeName() string { return "state.test.channelWaiter" }
func (*channelWaiter) StateFields() []string { return []string{"Queue"} }
func (v *channelWaiter) StateSave(s Sink)    { s.Save(0, &v.Queue) }
func (v *channelWaiter) StateLoad(_ context.Context, s Source) {
	s.LoadWait(0, &v.Queue)
	s.AfterLoad(func() {
		n := <-v.Queue
		v.Loaded = n.Value == 42 && n.loaded
	})
}

func init() { Register((*channelWaiter)(nil)) }

func TestChannelAfterLoad(t *testing.T) {
	c := make(chan graphNode, 1)
	c <- graphNode{Value: 42}
	close(c)
	src := channelWaiter{Queue: c}
	var dst channelWaiter
	roundtrip(t, &src, &dst)
	if !dst.Loaded || len(c) != 1 {
		t.Fatal("callback did not see restored queue contents")
	}
}

//go:nosplit
//go:norace
func channelHasWaiters(c *runtimeChannel) bool {
	channelLock(unsafe.Pointer(&c.lock))
	waiting := c.recvq[0] != nil || c.sendq[0] != nil
	channelUnlock(unsafe.Pointer(&c.lock))
	return waiting
}

func TestChannelRejectWaiters(t *testing.T) {
	for _, sender := range []bool{false, true} {
		c := make(chan int)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if sender {
				c <- 42
			} else {
				<-c
			}
		}()
		func() {
			defer func() {
				if sender {
					<-c
				} else {
					close(c)
				}
				<-done
			}()
			h := channelHeader(reflect.ValueOf(c))
			deadline := time.Now().Add(5 * time.Second)
			for !channelHasWaiters(h) {
				if time.Now().After(deadline) {
					t.Fatal("goroutine did not reach channel wait queue")
				}
				runtime.Gosched()
			}
			if _, _, err := Save(context.Background(), make([]byte, 4096), &c); err == nil || !strings.Contains(err.Error(), "waiting goroutines") {
				t.Fatalf("waiting goroutine: %v", err)
			}
		}()
	}
}

func TestChannelRejectTimer(t *testing.T) {
	t.Setenv("GODEBUG", "asynctimerchan=0")
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	c := timer.C
	if _, _, err := Save(context.Background(), make([]byte, 4096), &c); err == nil || !strings.Contains(err.Error(), "timer-backed") {
		t.Fatalf("timer channel: %v", err)
	}
}

func TestChannelInvalidContents(t *testing.T) {
	c := make(chan int, 1)
	c <- 42
	ds := decodeState{}
	if err := safely(func() {
		ds.decodeChannel(nil, reflect.ValueOf(c), &channelData{Values: arrayValue{Contents: []object{intValue(1)}}})
	}); err == nil || !strings.Contains(err.Error(), "available capacity") {
		t.Fatalf("duplicate channel payload: %v", err)
	}
	if <-c != 42 {
		t.Fatal("invalid payload modified an occupied slot")
	}
}

func TestChannelRejectSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := make(chan int, 1)
		if _, _, err := Save(context.Background(), make([]byte, 4096), &c); err == nil || !strings.Contains(err.Error(), "synctest") {
			t.Fatalf("synctest channel: %v", err)
		}
	})
}
