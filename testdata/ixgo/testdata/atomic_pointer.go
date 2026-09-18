package main

import "sync/atomic"

type Node struct {
	Value int
	Next  *Node
}

func Prepare() func() int {
	n := &Node{Value: 40}
	n.Next = n
	original := n
	var pointer atomic.Pointer[Node]
	var empty atomic.Pointer[Node]
	var field atomic.Pointer[int]
	var indirect atomic.Pointer[*Node]
	var text atomic.Pointer[string]
	label := "host"
	pointer.Store(n)
	field.Store(&n.Value)
	indirect.Store(&n)
	text.Store(&label)
	alias := &pointer
	var calls atomic.Int64
	return func() int {
		if pointer.Load() != n || alias != &pointer || alias.Load() != n || n.Next != n {
			panic("atomic target, wrapper alias or cycle changed")
		}
		if empty.Load() != nil || field.Load() != &n.Value || indirect.Load() != &n || *indirect.Load() != n {
			panic("nil, interior pointer or pointer-to-pointer changed")
		}
		if text.Load() != &label || *text.Load() != "host" || n.Value != 40+int(calls.Load()) {
			panic("atomic target value or type changed")
		}
		if alias.Swap(nil) != n || pointer.Load() != nil || alias.Swap(n) != nil {
			panic("restored Swap failed")
		}
		*field.Load()++
		count := int(calls.Add(1))
		if count == 2 {
			next := &Node{Value: n.Value}
			next.Next = next
			if !pointer.CompareAndSwap(n, next) {
				panic("restored CompareAndSwap failed")
			}
			n = next
			field.Store(&n.Value)
		}
		if count >= 2 && original.Value != 42 {
			panic("original target mutation was lost")
		}
		return count
	}
}
