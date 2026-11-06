//go:build !linux && !darwin

package main

func osRelease() string { return "unknown" }
