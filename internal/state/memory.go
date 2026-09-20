// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package state

import "io"

// writer encodes into caller-owned memory or an output stream.
type writer struct {
	mem []byte
	pos int
	out io.Writer
}

func (w *writer) put(obj object) error {
	return safely(func() { saveObject(w, obj) })
}

func (w *writer) writeBytes(p []byte) {
	if w.out != nil {
		n, err := w.out.Write(p)
		w.pos += n
		if err != nil {
			panic(err)
		}
		if n != len(p) {
			panic(io.ErrShortWrite)
		}
		return
	}
	if len(p) > len(w.mem)-w.pos {
		panic(io.ErrShortBuffer)
	}
	w.pos += copy(w.mem[w.pos:], p)
}

func (w *writer) writeString(s string) {
	if w.out != nil {
		n, err := io.WriteString(w.out, s)
		w.pos += n
		if err != nil {
			panic(err)
		}
		if n != len(s) {
			panic(io.ErrShortWrite)
		}
		return
	}
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
