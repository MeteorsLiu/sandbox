package main

import "gvisor.dev/gvisor/pkg/sentry/arch"

func setSyscall(ac *arch.Context64, number uint64, args [6]uint64) {
	ac.Regs.Orig_rax = number
	ac.Regs.Rdi, ac.Regs.Rsi, ac.Regs.Rdx = args[0], args[1], args[2]
	ac.Regs.R10, ac.Regs.R8, ac.Regs.R9 = args[3], args[4], args[5]
}
