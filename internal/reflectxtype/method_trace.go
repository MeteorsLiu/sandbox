package reflectxtype

import (
	"fmt"
	"os"
	"reflect"
	"unsafe"

	"github.com/goplus/reflectx"
)

// Temporary diagnostics for method descriptors crossing a state round trip.
// All callers hold transferMu, including accesses to the phase and seen map.
var methodTraceEnabled = os.Getenv("SANDBOX_TRACE_METHODS") != ""
var methodTracePhase string
var methodTraceSeen = make(map[unsafe.Pointer]string)

func traceMethods(format string, args ...any) {
	if methodTraceEnabled {
		line := fmt.Sprintf("METHODTRACE pid=%d phase=%s ", os.Getpid(), methodTracePhase) + fmt.Sprintf(format, args...) + "\n"
		_, _ = os.Stderr.WriteString(line)
	}
}

func traceMethodTable(event string, typ reflect.Type, raw []runtimeMethod) {
	if !methodTraceEnabled || len(raw) == 0 {
		return
	}
	rt := (*[2]unsafe.Pointer)(unsafe.Pointer(&typ))[1]
	entries := fmt.Sprintf("%+v", raw)
	if event == "scan" && methodTraceSeen[rt] == entries {
		return
	}
	methodTraceSeen[rt] = entries
	traceMethods("event=%s type=%q pkg=%q kind=%s addr=%p methods=%s", event, typ.String(), typ.PkgPath(), typ.Kind(), rt, entries)
	for index, method := range raw {
		if method.signature >= 0 || method.signature == -1 {
			continue
		}
		reflectOffsetsLock()
		ptr, found := reflectOffsets.m[method.signature]
		reflectOffsetsUnlock()
		if ptr != nil {
			continue
		}
		traceMethods("invalid-signature type=%q addr=%p index=%d signature=%d found=%t ptr=%p", typ.String(), rt, index, method.signature, found, ptr)
		if typ.Kind() == reflect.Struct {
			for i := range typ.NumField() {
				field := typ.Field(i)
				if !field.Anonymous {
					continue
				}
				base := (*[2]unsafe.Pointer)(unsafe.Pointer(&field.Type))[1]
				for j, entry := range runtimeMethods(base) {
					if entry.signature == -1 || entry.signature == 0 {
						name := reflectx.MethodX(field.Type, j).Name
						traceMethods("embedded-stripped type=%q addr=%p field=%q embeddedType=%q index=%d name=%q method=%+v", typ.String(), rt, field.Name, field.Type.String(), j, name, entry)
					}
				}
			}
		}
	}
}
