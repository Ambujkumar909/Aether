package main

import (
	"time"

	"golang.org/x/sys/windows"
)

// processCPU is this process's CPU time so far (user + kernel).
func processCPU() time.Duration {
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u) != nil {
		return 0
	}
	ft := func(f windows.Filetime) time.Duration {
		return time.Duration(int64(f.HighDateTime)<<32|int64(f.LowDateTime)) * 100
	}
	return ft(k) + ft(u)
}
