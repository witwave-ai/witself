package main

import (
	"context"
	"os"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func serverRuntimeConfigFromEnv() (server.Config, runtimeconfig.Config, error) {
	runtime, err := runtimeconfig.FromEnv("server", os.LookupEnv, os.Environ())
	if err != nil {
		return server.Config{}, runtimeconfig.Config{}, err
	}
	cfg := server.ConfigFromEnv()
	if runtime.Local {
		cfg.LocalRuntime = true
		cfg.APIAddr = runtime.APIAddr
		cfg.HealthAddr = runtime.HealthAddr
		cfg.MetricsAddr = runtime.MetricsAddr
		cfg.SpoolDir = runtime.SpoolDir
		cfg.SpoolRoot, err = runtimeconfig.OpenSpoolRoot(runtime.SpoolDir)
		if err != nil {
			return server.Config{}, runtimeconfig.Config{}, err
		}
	}
	return cfg, runtime, nil
}

func configureLocalStore(ctx context.Context, cfg *server.Config, st runtimeconfig.BootstrapStore, expectedAccountID string) error {
	if err := runtimeconfig.ValidateAccount(ctx, st, expectedAccountID); err != nil {
		return err
	}
	cfg.AppliedSchemaVersion = st.AppliedSchemaVersion
	cfg.Ready = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return runtimeconfig.CheckReady(ctx, st, store.SchemaVersion(), expectedAccountID)
	}
	return nil
}
