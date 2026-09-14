package main

import "gvisor.dev/gvisor/pkg/sentry/arch"

func setSyscall(ac *arch.Context64, number uint64, args [6]uint64) {
	ac.Regs.Regs[8] = number
	for i, arg := range args {
		ac.Regs.Regs[i] = arg
	}
}
