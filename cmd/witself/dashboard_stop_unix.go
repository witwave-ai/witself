//go:build !windows

package main

import (
	"context"
	"os"
	"syscall"

	"github.com/witwave-ai/witself/internal/dashboard"
)

const dashboardStopRequestName = "SIGINT"

func signalDashboardProcess(entry dashboard.RegistryEntry) error {
	process, err := os.FindProcess(entry.PID)
	if err != nil {
		return err
	}
	defer func() { _ = process.Release() }()
	return process.Signal(syscall.SIGINT)
}

func registerDashboardStop(ctx context.Context, _ dashboard.RegistryEntry) (context.Context, func(), error) {
	return ctx, func() {}, nil
}
