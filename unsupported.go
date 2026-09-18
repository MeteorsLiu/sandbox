//go:build !linux || (!arm64 && !amd64) || !cgo

package sandbox

import "errors"

func (*Sandbox) Run(func()) error {
	return errors.New("sandbox requires Linux, amd64 or arm64, and cgo")
}

func (*Sandbox) Close() error { return nil }
