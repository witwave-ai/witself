// Command lifecyclefixture is a synthetic lifecycle test peer. It implements
// only the tool invocations needed to exercise the supervisor; no SQL is stored.
package main

import (
	"errors" // FLAKE-3 fix 5 (W1)
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func main() {
	if runFixtureStopHelper() {
		return
	}
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		if name == "postgres" {
			fmt.Println("postgres (PostgreSQL) 16.1")
		} else {
			fmt.Println("localcell synthetic lifecycle fixture")
		}
		return
	}
	if err := run(name); err != nil { // FLAKE-3 fix 5 (W1)
		message, code := "synthetic lifecycle fixture failed", 1          // FLAKE-3 fix 5 (W1)
		if name == "pg_ctl" && len(os.Args) > 1 && os.Args[1] == "stop" { // FLAKE-3 fix 5 (W1)
			message, code = fixtureStopFailure(err) // FLAKE-3 fix 5 (W1)
		} // FLAKE-3 fix 5 (W1)
		fmt.Fprintln(os.Stderr, message) // FLAKE-3 fix 5 (W1)
		os.Exit(code)                    // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
}
func fixtureStopFailure(err error) (string, int) { // FLAKE-3 fix 5 (W1)
	var exit *exec.ExitError                                                         // FLAKE-3 fix 5 (W1)
	if errors.As(err, &exit) && (exit.ExitCode() == 124 || exit.ExitCode() == 125) { // FLAKE-3 fix 5 (W1)
		return "synthetic lifecycle fixture failed", exit.ExitCode() // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
	switch err.Error() { // FLAKE-3 fix 5 (W1)
	case "fixture PostgreSQL shutdown timeout": // FLAKE-3 fix 5 (W1)
		return "fixture PostgreSQL shutdown timeout", 1 // FLAKE-3 fix 5 (W1)
	case "fixture process creation identity mismatch": // FLAKE-3 fix 5 (W1)
		return "fixture process creation identity mismatch", 1 // FLAKE-3 fix 5 (W1)
	case "fixture process identity mismatch": // FLAKE-3 fix 5 (W1)
		return "fixture process identity mismatch", 1 // FLAKE-3 fix 5 (W1)
	default: // FLAKE-3 fix 5 (W1)
		return "synthetic lifecycle fixture failed", 1 // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
} // FLAKE-3 fix 5 (W1)

func run(name string) error {
	switch name {
	case "initdb":
		directory := argument("-D")
		if err := fixtureMkdir(directory); err != nil {
			return err
		}
		return fixtureWriteFile(filepath.Join(directory, "PG_VERSION"), []byte("16\n"))
	case "createdb":
		return fixtureWriteFile(filepath.Join("pg", "fixture-created-database"), []byte(os.Args[len(os.Args)-1]))
	case "psql":
		return runFixturePSQL() // FLAKE-3 fix 2 (F6)
	case "pg_ctl":
		return controlPostgres()
	case "postgres":
		return servePostgres()
	case "witself-server", "witself-worker":
		return serveComponent(strings.TrimPrefix(name, "witself-"))
	default:
		return fmt.Errorf("unknown fixture tool")
	}
}

func argument(name string) string {
	for i, value := range os.Args {
		if value == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func checkDatabase(database string) error {
	created, err := os.ReadFile(filepath.Join("pg", "fixture-created-database"))
	if err != nil || database == "" || database != string(created) {
		return fmt.Errorf("fixture connection did not select the created database")
	}
	return nil
}

func record(event string) error {
	file, err := fixtureOpenFile("fixture-events", os.O_APPEND|os.O_CREATE|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = fmt.Fprintln(file, event)
	return err
}

func controlPostgres() error {
	data := argument("-D")
	pidfile := filepath.Join(data, "postmaster.pid")
	if len(os.Args) > 1 && os.Args[1] == "start" {
		command := exec.Command(filepath.Join(filepath.Dir(os.Args[0]), fixtureExecutable("postgres")), "-D", data)
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if err := command.Start(); err != nil {
			return err
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(pidfile); err == nil {
				return command.Process.Release()
			}
		}
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("fixture PostgreSQL startup timeout")
	}
	dataBytes, err := os.ReadFile(pidfile)
	if err != nil {
		return err
	}
	// Real pg_ctl stop waits for the postmaster to exit. The pidfile can be
	// removed before deferred cleanup finishes, so it is not an exit witness.
	return stopFixturePostgres(dataBytes, time.Now().Add(5*time.Second))
}

func servePostgres() error {
	data := argument("-D")
	config, err := os.ReadFile(filepath.Join(data, "postgresql.conf"))
	if err != nil {
		return err
	}
	match := regexp.MustCompile(`(?m)^port = ([0-9]+)$`).FindSubmatch(config)
	if len(match) != 2 {
		return fmt.Errorf("fixture PostgreSQL port absent")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:"+string(match[1]))
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	stopped := make(chan os.Signal, 1)
	notifyFixtureStop(stopped)
	defer signal.Stop(stopped)
	if err := record("postgres-start"); err != nil {
		return err
	}
	pidfile := filepath.Join(data, "postmaster.pid")
	epoch := time.Now()
	if delay, err := os.ReadFile("fixture-postgres-epoch-delay"); err == nil {
		offset, err := time.ParseDuration(string(delay))
		if err != nil || offset < 0 {
			return fmt.Errorf("fixture PostgreSQL epoch delay is invalid")
		}
		// Model work between process creation and PostgreSQL's epoch capture
		// without introducing an actual sleep into the lifecycle fixture.
		epoch = epoch.Add(offset)
	}
	recordData := fixturePostmasterRecord(data, string(match[1]), epoch.Unix())
	_, emptyErr := os.Stat("fixture-postgres-empty-record")
	if emptyErr == nil {
		recordData = nil
	}
	// pg_ctl treats existence as readiness. Publish the complete record in one
	// rename so it can never return while the PID file is empty or partial.
	// The explicit empty-record negative control is also published atomically.
	if err := fixtureWriteFile(pidfile+".pending", recordData); err != nil {
		return err
	}
	if argument("--fixture-publication-gate") == "held" {
		// This explicit fixture-only argument never enters production launch
		// paths. The release signal is always scoped to this fixture's root.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat("fixture-postgres-publication-release"); err == nil {
				break
			} else if !os.IsNotExist(err) {
				return err
			}
			if !time.Now().Before(deadline) {
				if err := fixtureWriteFile("fixture-postgres-publication-expired", nil); err != nil { // FLAKE-3 fix 4 (F9)
					return err // FLAKE-3 fix 4 (F9)
				} // FLAKE-3 fix 4 (F9)
				return fmt.Errorf("fixture PostgreSQL publication timeout")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := os.Rename(pidfile+".pending", pidfile); err != nil {
		return err
	}
	if emptyErr == nil {
		// No process authority can be recovered from this deliberately invalid
		// record. Exit immediately so the negative control cannot strand a peer.
		return nil
	}
	<-stopped
	if err := record("postgres-stop"); err != nil {
		return err
	}
	if err := os.Remove(pidfile); err != nil {
		return err
	}
	if _, err := os.Stat("fixture-postgres-exit-delay"); err == nil {
		// Keep the process and its listener alive after removing the pidfile.
		// This deterministic fixture seam exposes premature pg_ctl completion.
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func serveComponent(component string) error {
	if component == "server" {
		if _, err := os.Stat("fixture-server-exit"); err == nil {
			fmt.Fprintln(os.Stderr, "postgres://synthetic-private-value@invalid/fixture")
			fmt.Fprintln(os.Stderr, "witself-server: migration wait exceeded 250ms during advisory lock")
			os.Exit(7)
		}
		if data, err := os.ReadFile("fixture-server-delay"); err == nil {
			delay, err := time.ParseDuration(string(data))
			if err != nil {
				return err
			}
			time.Sleep(delay)
		}
	}
	dsn, err := url.Parse(os.Getenv("WITSELF_DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("fixture database connection URL is invalid")
	}
	if err := checkDatabase(strings.TrimPrefix(dsn.Path, "/")); err != nil {
		return err
	}
	addresses := []string{os.Getenv("WITSELF_HEALTH_ADDR"), os.Getenv("WITSELF_METRICS_ADDR")}
	if component == "server" {
		addresses = append(addresses, os.Getenv("WITSELF_API_ADDR"))
	}
	listeners := make([]net.Listener, 0, len(addresses))
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, address := range addresses {
		listener, err := net.Listen("tcp4", address)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
	}
	stopped := make(chan os.Signal, 1)
	notifyFixtureStop(stopped)
	defer signal.Stop(stopped)
	if err := record(component + "-start"); err != nil {
		return err
	}
	for _, listener := range listeners {
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})}
		go func() { _ = server.Serve(listener) }()
	}
	<-stopped
	return record(component + "-stop")
}
