// Command localcellharness is a developer-only driver for disposable local
// cells. It is not an installer and is not registered by any existing binary.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		// Build output and connection errors can include caller-owned values.
		// The harness exposes only the lifecycle package's value-free errors.
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	return runWithBuilder(ctx, args, out, buildBinaries)
}

func runWithBuilder(ctx context.Context, args []string, out io.Writer, build func(context.Context, string, string, *localcell.Binaries) error) error {
	flags := flag.NewFlagSet("localcellharness", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	operation := flags.String("operation", "", "install, start, stop, inspect, status, repair, or remove")
	root := flags.String("root", "", "explicit disposable cell directory")
	pgbin := flags.String("pgbin", os.Getenv("WITSELF_LOCALCELL_PGBIN"), "PostgreSQL binary directory")
	server := flags.String("server", "", "server binary; omitted builds from source")
	worker := flags.String("worker", "", "worker binary; omitted builds from source")
	source := flags.String("source", ".", "source checkout used to build missing binaries")
	jobs := flags.String("jobs", "", "comma-separated local worker jobs")
	timeout := flags.Duration("timeout", 45*time.Second, "per-operation readiness and shutdown deadline")
	leaveRunning := flags.Bool("leave-running", false, "leave PostgreSQL running after install")
	purge := flags.Bool("purge", false, "explicitly delete the local database and manifest during remove")
	interrupt := flags.Bool("interrupt-after-postgres", false, "test-only interruption during start")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("localcellharness: invalid arguments")
	}
	if !validOperation(*operation) || *root == "" || !filepath.IsAbs(*root) || *timeout <= 0 {
		return errors.New("localcellharness: an operation, absolute root, and positive timeout are required")
	}
	if (*purge && *operation != "remove") || (*leaveRunning && *operation != "install") || (*interrupt && *operation != "start") {
		return errors.New("localcellharness: flag does not apply to this operation")
	}
	binaries := localcell.Binaries{PostgresDir: *pgbin, Server: *server, Worker: *worker}
	if (*operation == "install" || *operation == "start") && (binaries.Server == "" || binaries.Worker == "") {
		buildDir, err := harnessBuildDirectory(ctx, *root)
		if err != nil {
			return errors.New("localcellharness: cannot create component build directory")
		}
		// Children and later stop/repair invocations still need these executable
		// paths after this command returns. Only a successful purge removes them.
		if err := build(ctx, *source, buildDir, &binaries); err != nil {
			_ = os.RemoveAll(buildDir)
			return err
		}
	}
	config := localcell.Config{Root: *root, Binaries: binaries, Timeout: *timeout}
	if *jobs != "" {
		config.Jobs = strings.Split(*jobs, ",")
	}
	if *interrupt {
		config.Failpoint = func(operation, step string) error {
			if operation == "start" && step == "postgres" {
				return errors.New("localcellharness: injected interruption")
			}
			return nil
		}
	}
	cell, err := localcell.New(config)
	if err != nil {
		return err
	}
	switch *operation {
	case "install":
		err = cell.Install(ctx, localcell.InstallOptions{LeaveRunning: *leaveRunning})
	case "start":
		err = cell.Start(ctx)
	case "stop":
		err = cell.Stop(ctx)
	case "repair":
		err = cell.Repair(ctx)
	case "remove":
		err = cell.Remove(ctx, localcell.RemoveOptions{Purge: *purge})
		if err == nil && *purge {
			cache := filepath.Join(*root, ".harness-build")
			if _, statErr := os.Lstat(cache); !errors.Is(statErr, os.ErrNotExist) {
				if statErr != nil || harnessPrivateDirectory(cache) != nil || os.RemoveAll(cache) != nil {
					return errors.New("localcellharness: cannot remove component build directory")
				}
			}
		}
	case "inspect":
		inspection, inspectErr := cell.Inspect(ctx)
		if inspectErr != nil {
			return inspectErr
		}
		return json.NewEncoder(out).Encode(inspection)
	case "status":
		status, statusErr := cell.Status(ctx)
		if statusErr != nil {
			return statusErr
		}
		return json.NewEncoder(out).Encode(status)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Operation string `json:"operation"`
		State     string `json:"state"`
	}{*operation, "done"})
}

func validOperation(operation string) bool {
	switch operation {
	case "install", "start", "stop", "inspect", "status", "repair", "remove":
		return true
	default:
		return false
	}
}

func harnessBuildDirectory(ctx context.Context, root string) (string, error) {
	if err := harnessPrivateDirectory(root); err != nil {
		return "", err
	}
	cell, err := localcell.New(localcell.Config{Root: root})
	if err != nil {
		return "", err
	}
	if _, err := cell.Inspect(ctx); err != nil {
		return "", err
	}
	cache := filepath.Join(root, ".harness-build")
	if err := harnessPrivateDirectory(cache); err != nil {
		return "", err
	}
	for range 10 {
		var suffix [6]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", errors.New("cannot allocate component build identity")
		}
		directory := filepath.Join(cache, fmt.Sprintf("build-%x", suffix))
		if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := harnessPrivateDirectory(directory); err != nil {
			return "", err
		}
		return directory, nil
	}
	return "", errors.New("cannot allocate component build directory")
}

func harnessExecutable(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func buildBinaries(ctx context.Context, source, directory string, binaries *localcell.Binaries) error {
	for _, component := range []struct {
		name string
		path *string
	}{{"witself-server", &binaries.Server}, {"witself-worker", &binaries.Worker}} {
		if *component.path != "" {
			continue
		}
		path := filepath.Join(directory, harnessExecutable(component.name))
		// Build tools do not promise PW1's protected DACL. Publish into a private
		// file before making the path available to a lifecycle invocation.
		staging, err := os.MkdirTemp("", "lc-build-")
		if err != nil {
			return errors.New("localcellharness: cannot create build staging directory")
		}
		defer func() { _ = os.RemoveAll(staging) }()
		staged := filepath.Join(staging, harnessExecutable(component.name))
		cmd := exec.CommandContext(ctx, "go", "build", "-o", staged, "./cmd/"+component.name)
		// Reuse the caller's already-installed toolchain and cached modules;
		// the developer harness never downloads a component or dependency.
		cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
		cmd.Dir = source
		// Never copy build diagnostics to harness output: source and toolchain
		// errors are not part of the lifecycle's value-free output contract.
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Run(); err != nil {
			return errors.New("localcellharness: component build failed")
		}
		if err := publishHarnessBinary(staged, path); err != nil {
			return errors.New("localcellharness: cannot publish component build")
		}
		*component.path = path
	}
	return nil
}

func publishHarnessBinary(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := harnessCreateFile(target)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	closeErr := output.Close()
	if err != nil {
		return err
	}
	return closeErr
}
