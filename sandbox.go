// Package sandbox executes a Go closure in Sentry and copies changes to its
// captured object graph back to the caller. See README.md for transfer limits.
package sandbox

// Syscall is a trapped guest syscall, observed after Context.Switch returns and
// before Sentry dispatches it. Addresses in Args belong to the guest.
type Syscall struct {
	Number uint64
	Args   [6]uint64
}

// Sandbox selects the shared library and the synchronous host inspector.
// Library defaults to sentrylib.so beside the calling executable.
type Sandbox struct {
	Library string
	Inspect func(*Syscall)
}

// Run executes fn using the default Sandbox.
func Run(fn func()) error { return new(Sandbox).Run(fn) }
