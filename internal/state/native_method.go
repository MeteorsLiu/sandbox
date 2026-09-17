package state

import (
	"reflect"
	"strings"
	"unsafe"
)

// Go 1.26.6 abi.Method. Reading only names avoids constructing reflect.FuncOf
// types (and populating reflect's global caches) while indexing static methods.
type nativeMethod struct {
	name, signature, ifn, tfn int32
}

//go:linkname nativeMethods github.com/goplus/reflectx.rtypeMethods
func nativeMethods(unsafe.Pointer) []nativeMethod

//go:linkname nativeMethodName reflect.resolveNameOff
func nativeMethodName(unsafe.Pointer, int32) unsafe.Pointer

//go:linkname nativeMethodPackage reflect.pkgPath
func nativeMethodPackage(struct{ bytes *byte }) string

func (m *nativeMetadata) indexMethods(receiver reflect.Type) {
	base, recv := receiver, receiver.Name()
	if receiver.Kind() == reflect.Pointer {
		base = receiver.Elem()
		recv = "(*" + base.Name() + ")"
	}
	pkg := base.PkgPath()
	prefix := nativePackagePrefix(pkg) + "." + recv + "."
	add := func(name, methodPkg string) {
		// cmd/compile/internal/ir.MethodSymSuffix qualifies private methods
		// imported through embedding: a.(*T).example.org/b.hidden.
		symbol := prefix
		if methodPkg != "" && methodPkg != pkg {
			symbol += nativePackagePrefix(methodPkg) + "."
		}
		symbol += name
		_, direct := m.names[symbol]
		_, bound := m.names[symbol+"-fm"]
		if !direct && !bound {
			return
		}
		if previous, ok := m.methods[symbol]; ok && previous != receiver {
			// Distinct function-local types can have identical package/name
			// metadata. Do not choose a receiver by traversal order.
			m.methods[symbol] = nil
		} else {
			m.methods[symbol] = receiver
		}
	}
	if receiver.Kind() == reflect.Interface {
		for i := 0; i < receiver.NumMethod(); i++ {
			method := receiver.Method(i)
			add(method.Name, method.PkgPath)
		}
		return
	}
	rt := (*[2]unsafe.Pointer)(unsafe.Pointer(&receiver))[1]
	for _, method := range nativeMethods(rt) {
		name := nativeMethodName(rt, method.name)
		// abi.Name stores flags, a varint byte length, then the UTF-8 name.
		var length, shift uintptr
		offset := uintptr(1)
		for {
			b := *(*byte)(unsafe.Add(name, offset))
			offset++
			length |= uintptr(b&0x7f) << shift
			if b&0x80 == 0 {
				break
			}
			shift += 7
		}
		methodPkg := ""
		if *(*byte)(name)&1 == 0 {
			methodPkg = nativeMethodPackage(struct{ bytes *byte }{(*byte)(name)})
		}
		add(unsafe.String((*byte)(unsafe.Add(name, offset)), length), methodPkg)
	}
}

// Follow cmd/internal/objabi.PathToPrefix, not URL path escaping. Dots in
// earlier path components stay literal; example.org/pkg.v1 becomes
// example.org/pkg%2ev1. The complete prefix is matched, never split on dots.
func nativePackagePrefix(path string) string {
	lastSlash := strings.LastIndexByte(path, '/')
	var out strings.Builder
	for i := 0; i < len(path); i++ {
		b := path[i]
		if b <= ' ' || b == '%' || b == '"' || b >= 0x7f || b == '.' && i > lastSlash {
			const hex = "0123456789abcdef"
			out.WriteByte('%')
			out.WriteByte(hex[b>>4])
			out.WriteByte(hex[b&15])
		} else {
			out.WriteByte(b)
		}
	}
	return out.String()
}
