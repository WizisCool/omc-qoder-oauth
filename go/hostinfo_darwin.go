//go:build darwin

package main

import "syscall"

func osRelease() string {
	s, err := syscall.Sysctl("kern.osrelease")
	if err != nil || s == "" {
		return "unknown"
	}
	return s
}
