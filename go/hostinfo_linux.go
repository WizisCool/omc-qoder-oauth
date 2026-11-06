//go:build linux

package main

import "syscall"

func osRelease() string {
	var u syscall.Utsname
	if syscall.Uname(&u) != nil {
		return "unknown"
	}
	b := make([]byte, 0, len(u.Release))
	for _, v := range u.Release {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return string(b)
}
