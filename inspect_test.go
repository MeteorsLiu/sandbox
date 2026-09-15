package sandbox

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLazyInspectionAndLifetime(t *testing.T) {
	reads, decodes := 0, 0
	access := &syscallAccess{
		active: true,
		read: func(_ uint64, dst []byte) (int, error) {
			reads++
			return copy(dst, "data"), nil
		},
		decode: func(_ *Syscall, _ int) ([]byte, error) {
			decodes++
			return []byte(`{"number":1,"name":"test","args":[{"format":"Hex","value":18446744073709551615,"decoded":true}]}`), nil
		},
		rewrite: func(_ *Syscall, data []byte) error {
			if !strings.Contains(string(data), "18446744073709551615") {
				t.Fatal("64-bit argument lost precision")
			}
			return nil
		},
	}
	call := &Syscall{Number: 1, Name: "test", access: access}
	if call.Name != "test" || reads != 0 || decodes != 0 {
		t.Fatal("filtering unexpectedly decoded memory")
	}
	decoded, err := call.Decode(1024)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Args[0].Value.(json.Number).String() != "18446744073709551615" {
		t.Fatal("argument precision lost")
	}
	if err := call.Rewrite(decoded); err != nil {
		t.Fatal(err)
	}
	access.active = false
	if _, err := call.ReadMemory(1, make([]byte, 4)); err == nil {
		t.Fatal("retained callback could access guest memory")
	}
	if _, err := call.Decode(1024); err == nil {
		t.Fatal("retained callback could decode arguments")
	}
	if err := call.Rewrite(decoded); err == nil {
		t.Fatal("retained callback could rewrite arguments")
	}
}
