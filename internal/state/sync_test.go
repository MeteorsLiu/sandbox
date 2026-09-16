package state

import (
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAtomicScalars(t *testing.T) {
	type values struct {
		Bool    atomic.Bool
		Int32   atomic.Int32
		Int64   atomic.Int64
		Uint32  atomic.Uint32
		Uint64  atomic.Uint64
		Uintptr atomic.Uintptr
	}
	src := new(values)
	src.Bool.Store(true)
	src.Int32.Store(-42)
	src.Int64.Store(-1 << 40)
	src.Uint32.Store(1 << 31)
	src.Uint64.Store(1 << 63)
	src.Uintptr.Store(123)
	dst := new(values)
	roundtrip(t, src, dst)
	if !dst.Bool.Load() || dst.Int32.Load() != -42 || dst.Int64.Load() != -1<<40 || dst.Uint32.Load() != 1<<31 || dst.Uint64.Load() != 1<<63 || dst.Uintptr.Load() != 123 {
		t.Fatal("atomic scalar value changed")
	}
	if !dst.Bool.CompareAndSwap(true, false) || dst.Int32.Add(2) != -40 || dst.Uint64.Swap(7) != 1<<63 || !src.Bool.Load() || src.Int32.Load() != -42 {
		t.Fatal("restored atomic operations failed or changed the source")
	}
	zero := new(values)
	roundtrip(t, zero, dst)
	if dst.Bool.Load() || dst.Int32.Load() != 0 || dst.Int64.Load() != 0 || dst.Uint32.Load() != 0 || dst.Uint64.Load() != 0 || dst.Uintptr.Load() != 0 {
		t.Fatal("zero atomics did not replace previous values")
	}
}

func TestAtomicPointerGraph(t *testing.T) {
	type root struct {
		Node  atomic.Pointer[graphNode]
		Field atomic.Pointer[int64]
		Other *graphNode
		Alias *atomic.Pointer[graphNode]
		Nil   atomic.Pointer[graphNode]
	}
	n := &graphNode{Value: 42}
	n.Next = n
	src := &root{Other: n}
	src.Node.Store(n)
	src.Field.Store(&n.Value)
	src.Alias = &src.Node
	dst := new(root)
	roundtrip(t, src, dst)
	runtime.GC()
	got := dst.Node.Load()
	if got == n || got != dst.Other || got.Next != got || !got.loaded || dst.Field.Load() != &got.Value || dst.Alias != &dst.Node || dst.Nil.Load() != nil {
		t.Fatal("atomic target, interior pointer, wrapper alias or cycle changed")
	}
	if !dst.Node.CompareAndSwap(got, nil) || dst.Alias.Load() != nil || src.Node.Load() != n {
		t.Fatal("restored atomic pointer operations failed or changed the source")
	}
	*dst.Field.Load() = 99
	if got.Value != 99 || n.Value != 42 {
		t.Fatal("atomic target is not an independent graph")
	}
}

func TestAtomicPointerSelfReference(t *testing.T) {
	type node struct {
		Self atomic.Pointer[node]
		Next atomic.Pointer[*node]
	}
	src := new(node)
	src.Self.Store(src)
	src.Next.Store(&src)
	var dst *node
	roundtrip(t, &src, &dst)
	if dst == src || dst.Self.Load() != dst || dst.Next.Load() != &dst {
		t.Fatal("atomic self-reference or pointer-to-pointer changed")
	}
}

func TestAtomicValue(t *testing.T) {
	for _, input := range []any{nil, (*int)(nil), 42, "value", []int{1, 2}, map[string]int{"n": 3}, reflect.TypeFor[int](), reflect.ValueOf(42)} {
		src := new(atomic.Value)
		if input != nil {
			src.Store(input)
		}
		dst := new(atomic.Value)
		dst.Store("old")
		roundtrip(t, src, dst)
		if value, ok := input.(reflect.Value); ok {
			got, ok := dst.Load().(reflect.Value)
			if !ok || got.Type() != value.Type() || !reflect.DeepEqual(got.Interface(), value.Interface()) {
				t.Fatal("reflected atomic.Value changed its represented type or value")
			}
		} else if !reflect.DeepEqual(dst.Load(), input) {
			t.Fatalf("atomic.Value: got %T %#v, want %T %#v", dst.Load(), dst.Load(), input, input)
		}
		if input != nil {
			dst.Store(input)
		}
	}
	n := &graphNode{Value: 42}
	value := new(atomic.Value)
	value.Store(n)
	src := []any{value, n, value}
	var dst []any
	roundtrip(t, &src, &dst)
	got := dst[0].(*atomic.Value)
	if got != dst[2].(*atomic.Value) || got.Load() != dst[1].(*graphNode) || got.Load() == n || !got.Load().(*graphNode).loaded {
		t.Fatal("atomic.Value lost its target or alias")
	}
	self := new(atomic.Value)
	self.Store(self)
	restored := new(atomic.Value)
	roundtrip(t, self, restored)
	if restored.Load() != restored {
		t.Fatal("atomic.Value self-reference changed")
	}
}

func TestMutexZero(t *testing.T) {
	for _, locked := range []bool{false, true} {
		src := new(sync.Mutex)
		if locked {
			src.Lock()
		}
		dst := new(sync.Mutex)
		dst.Lock()
		roundtrip(t, src, dst)
		if !dst.TryLock() {
			t.Fatal("restored mutex is locked")
		}
		dst.Unlock()
		if !dst.TryLock() {
			t.Fatal("restored mutex cannot be reused")
		}
		dst.Unlock()
		if locked {
			if src.TryLock() {
				t.Fatal("snapshot unlocked the source")
			}
			src.Unlock()
		}
	}
}

func TestRWMutexZero(t *testing.T) {
	for _, readers := range []int{-1, 0, 2} {
		src := new(sync.RWMutex)
		if readers < 0 {
			src.Lock()
		} else {
			for i := 0; i < readers; i++ {
				src.RLock()
			}
		}
		dst := new(sync.RWMutex)
		dst.Lock()
		roundtrip(t, src, dst)
		if !dst.TryLock() {
			t.Fatal("restored RWMutex is locked")
		}
		dst.Unlock()
		if !dst.TryRLock() {
			t.Fatal("restored RWMutex cannot acquire read lock")
		}
		dst.RUnlock()
		switch {
		case readers < 0:
			if src.TryLock() || src.TryRLock() {
				t.Fatal("snapshot changed source write lock")
			}
			src.Unlock()
		case readers > 0:
			if src.TryLock() {
				t.Fatal("snapshot changed source read locks")
			}
			for i := 0; i < readers; i++ {
				src.RUnlock()
			}
		}
	}
}

func TestSyncPoolZero(t *testing.T) {
	calls := 0
	pool := &sync.Pool{New: func() any { calls++; return "host" }}
	pool.Put(make(chan int))
	src := []any{pool, pool}
	var dst []any
	roundtrip(t, &src, &dst)
	got := dst[0].(*sync.Pool)
	if got == pool || got != dst[1].(*sync.Pool) || got.New != nil || got.Get() != nil || calls != 0 || pool.New == nil {
		t.Fatal("pool was not emptied, lost its alias, or modified the source")
	}
	got.Put(42)
	got.New = func() any { return 42 }
	if got.Get() != 42 {
		t.Fatal("restored pool cannot be reused")
	}
	// A non-zero destination must also lose its old New hook and local cache.
	roundtrip(t, pool, got)
	if got.New != nil || got.Get() != nil {
		t.Fatal("previous destination pool contents survived")
	}
}

func TestSyncOnceZero(t *testing.T) {
	src, dst := new(sync.Once), new(sync.Once)
	src.Do(func() {})
	dst.Do(func() {})
	roundtrip(t, src, dst)
	calls := 0
	dst.Do(func() { calls++ })
	dst.Do(func() { calls++ })
	src.Do(func() { t.Fatal("snapshot reset the source Once") })
	if calls != 1 {
		t.Fatal("restored Once did not start fresh")
	}
}

func TestSyncWaitGroupZero(t *testing.T) {
	src, dst := new(sync.WaitGroup), new(sync.WaitGroup)
	src.Add(2)
	dst.Add(1)
	started, done := make(chan struct{}), make(chan struct{})
	go func() { close(started); src.Wait(); close(done) }()
	defer func() { src.Done(); src.Done(); <-done }()
	<-started
	roundtrip(t, src, dst)
	dst.Wait()
	dst.Add(1)
	dst.Done()
	dst.Wait()
	select {
	case <-done:
		t.Fatal("snapshot released the source WaitGroup")
	default:
	}
}

func TestSyncCondZero(t *testing.T) {
	src := sync.NewCond(new(sync.Mutex))
	started, done := make(chan struct{}), make(chan struct{})
	released := false
	go func() {
		src.L.Lock()
		close(started)
		for !released {
			src.Wait()
		}
		src.L.Unlock()
		close(done)
	}()
	defer func() {
		src.L.Lock()
		released = true
		src.Broadcast()
		src.L.Unlock()
		<-done
	}()
	<-started
	src.L.Lock()
	src.L.Unlock()
	dst := sync.NewCond(new(sync.Mutex))
	roundtrip(t, src, dst)
	if !reflect.ValueOf(dst).Elem().IsZero() {
		t.Fatal("restored Cond retained its locker or notification state")
	}
	select {
	case <-done:
		t.Fatal("snapshot released the source Cond")
	default:
	}
	dst.L = new(sync.Mutex)
	dst.L.Lock()
	dst.Signal()
	dst.Broadcast()
	dst.L.Unlock()
}

func TestSyncLockerAliases(t *testing.T) {
	type root struct {
		Mutex sync.Mutex
		First sync.Locker
		Other sync.Locker
	}
	src := new(root)
	src.Mutex.Lock()
	defer src.Mutex.Unlock()
	src.First, src.Other = &src.Mutex, &src.Mutex
	dst := new(root)
	roundtrip(t, src, dst)
	if dst.First != &dst.Mutex || dst.Other != &dst.Mutex || !dst.Mutex.TryLock() {
		t.Fatal("sync.Locker aliases no longer refer to the same fresh mutex")
	}
	dst.First.Unlock()
}

func TestMutexDropsWaiter(t *testing.T) {
	m := new(sync.Mutex)
	m.Lock()
	done := make(chan struct{})
	go func() {
		m.Lock()
		m.Unlock()
		close(done)
	}()
	defer func() { m.Unlock(); <-done }()
	state := (*int32)(reflect.ValueOf(m).Elem().FieldByName("mu").FieldByName("state").Addr().UnsafePointer())
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(state)>>3 == 0 {
		if time.Now().After(deadline) {
			t.Fatal("goroutine did not wait on mutex")
		}
		runtime.Gosched()
	}
	dst := new(sync.Mutex)
	roundtrip(t, m, dst)
	if !dst.TryLock() {
		t.Fatal("source waiters left the restored mutex locked")
	}
	dst.Unlock()
	select {
	case <-done:
		t.Fatal("snapshot released a source waiter")
	default:
	}
}

func TestRWMutexDropsWaiters(t *testing.T) {
	for _, reader := range []bool{false, true} {
		name := "writer"
		if reader {
			name = "reader"
		}
		t.Run(name, func(t *testing.T) {
			m := new(sync.RWMutex)
			if reader {
				m.Lock()
			} else {
				m.RLock()
			}
			done := make(chan struct{})
			go func() {
				if reader {
					m.RLock()
					m.RUnlock()
				} else {
					m.Lock()
					m.Unlock()
				}
				close(done)
			}()
			defer func() {
				if reader {
					m.Unlock()
				} else {
					m.RUnlock()
				}
				<-done
			}()
			count := (*int32)(reflect.ValueOf(m).Elem().FieldByName("readerCount").FieldByName("v").Addr().UnsafePointer())
			deadline := time.Now().Add(5 * time.Second)
			for atomic.LoadInt32(count) != -(1<<30)+1 {
				if time.Now().After(deadline) {
					t.Fatal("goroutine did not wait on RWMutex")
				}
				runtime.Gosched()
			}
			dst := new(sync.RWMutex)
			roundtrip(t, m, dst)
			if !dst.TryLock() {
				t.Fatal("source waiters left the restored RWMutex locked")
			}
			dst.Unlock()
			select {
			case <-done:
				t.Fatal("snapshot released a source waiter")
			default:
			}
		})
	}
}
