// Package sandbox executes a Go closure in Sentry and copies changes to its
// captured object graph back to the caller. See README.md for transfer limits.
package sandbox

// Syscall is a trapped guest syscall, observed after Context.Switch returns and
// before Sentry dispatches it. Addresses in Args belong to the guest.
type Syscall struct {
	Number uint64
	Args   [6]uint64
	Name   string
	access *syscallAccess
}

// Mount describes a filesystem in the guest namespace. Source is a host
// directory for bind mounts; overlay paths in Options refer to guest paths.
type Mount struct {
	Type    string   `json:"type"`
	Source  string   `json:"source,omitempty"`
	Target  string   `json:"target"`
	Options []string `json:"options,omitempty"`
}

// Sandbox selects the shared library, guest mounts, environment and inspector.
// Library defaults to sentrylib.so beside the calling executable.
type Sandbox struct {
	Library string
	// An empty Mounts uses a read-only host root and guest procfs. Otherwise
	// the list replaces all defaults, starting with a bind or tmpfs at /.
	Mounts  []Mount
	Inspect func(*Syscall)
	// Env contains the guest's KEY=value environment entries. Nil inherits
	// the host environment at each Run; a non-nil slice replaces it entirely.
	// An empty non-nil slice starts the guest with no environment variables.
	Env []string
}

// Run executes fn using the default Sandbox.
func Run(fn func()) error { return new(Sandbox).Run(fn) }
