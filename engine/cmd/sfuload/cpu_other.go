//go:build !windows

package main

import (
	"syscall"
	"time"
)

// processCPU is this process's CPU time so far (user + system).
func processCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
