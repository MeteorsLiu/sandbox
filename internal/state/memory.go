// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package state

import "io"

// writer fills caller-owned memory. Exhaustion returns an error without
// growing the buffer, so a shared mapping remains the destination throughout.
type writer struct {
	mem []byte
	pos int
}

func (w *writer) put(obj object) error {
	return safely(func() { saveObject(w, obj) })
}

func (w *writer) writeBytes(p []byte) {
	if len(p) > len(w.mem)-w.pos {
		panic(io.ErrShortBuffer)
	}
	w.pos += copy(w.mem[w.pos:], p)
}

func (w *writer) writeString(s string) {
	if len(s) > len(w.mem)-w.pos {
		panic(io.ErrShortBuffer)
	}
	w.pos += copy(w.mem[w.pos:], s)
}

type reader struct {
	mem []byte
	pos int
}

func (r *reader) get() (obj object, err error) {
	err = safely(func() { obj = loadObject(r) })
	return obj, err
}

func (r *reader) readBytes(n uint64) []byte {
	if n > uint64(len(r.mem)-r.pos) {
		panic(io.ErrUnexpectedEOF)
	}
	p := r.mem[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return p
}
