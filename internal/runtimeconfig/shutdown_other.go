//go:build !windows

package runtimeconfig

func localShutdownPathValid(_ string) bool { return true }
