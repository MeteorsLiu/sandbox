package state

import "reflect"

// Copying these fields would import OS descriptors, timers or runtime waiters.
// Synchronization values use syncFields instead; only their data is retained.
func checkProcessResource(typ reflect.Type) {
	if typ.Kind() != reflect.Struct {
		return
	}
	pkg, name := typ.PkgPath(), typ.Name()
	if pkg == "internal/poll" || pkg == "os" && (name == "File" || name == "file" || name == "Process") ||
		pkg == "time" && (name == "Timer" || name == "Ticker") ||
		pkg == "context" && (name == "cancelCtx" || name == "timerCtx" || name == "afterFuncCtx") {
		Failf("%v contains process-local state", typ)
	}
}
