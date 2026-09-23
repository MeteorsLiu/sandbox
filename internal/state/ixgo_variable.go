package state

import (
	"reflect"
	"sort"

	"github.com/goplus/ixgo"
)

// packageVariable identifies storage owned by the receiving Go package. Its
// contents never enter the graph: runtime.MemProfileRate must refer to the
// local runtime, and filepath.SkipDir must keep the local sentinel's identity.
type packageVariable struct {
	pkg, name stringValue
}

type packageVariableKey struct {
	typ     reflect.Type
	address uintptr
}

func (es *encodeState) findPackageVariable(obj reflect.Value) *packageVariable {
	if es.packageVariables == nil {
		es.packageVariables = make(map[packageVariableKey]packageVariable)
		// Package registrations must be complete before Save, just as for the
		// existing Package binding. Resolve declarations, never their values.
		for _, path := range ixgo.PackageList() {
			pkg, _ := ixgo.LookupPackage(path)
			names := make([]string, 0, len(pkg.Vars))
			for name := range pkg.Vars {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				value := pkg.Vars[name]
				if value.Kind() != reflect.Pointer || value.IsNil() {
					continue
				}
				key := packageVariableKey{value.Type(), value.Pointer()}
				if _, exists := es.packageVariables[key]; !exists {
					es.packageVariables[key] = packageVariable{stringValue(path), stringValue(name)}
				}
			}
		}
	}
	if variable, ok := es.packageVariables[packageVariableKey{obj.Type(), obj.Pointer()}]; ok {
		return &variable
	}
	return nil
}

func (v *packageVariable) resolve(typ reflect.Type) reflect.Value {
	pkg, ok := ixgo.LookupPackage(string(v.pkg))
	if !ok {
		Failf("ixgo variable package %q is not registered in this process", v.pkg)
	}
	value, ok := pkg.Vars[string(v.name)]
	if !ok {
		Failf("ixgo variable %q.%q is not registered in this process", v.pkg, v.name)
	}
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Type() != typ {
		Failf("ixgo variable %q.%q does not have pointer type %v", v.pkg, v.name, typ)
	}
	return value
}
