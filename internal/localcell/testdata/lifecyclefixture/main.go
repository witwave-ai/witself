// Command lifecyclefixture is a synthetic lifecycle test peer. It implements
// only the tool invocations needed to exercise the supervisor; no SQL is stored.
package main

import (
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
	if run(name) != nil {
		fmt.Fprintln(os.Stderr, "synthetic lifecycle fixture failed")
		os.Exit(1)
	}
}

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
		if err := checkDatabase(argument("-d")); err != nil {
			return err
		}
		if strings.Contains(argument("-c"), "SELECT id FROM accounts") {
			fmt.Println("acc_aaaaaaaaaaaaaaaa")
		} else {
			fmt.Println("1")
		}
		return nil
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
	if err := stopFixturePostgres(dataBytes); err != nil {
		return err
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(pidfile); os.IsNotExist(err) {
			return nil
		}
	}
	return fmt.Errorf("fixture PostgreSQL shutdown timeout")
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
	if err := fixtureWriteFile(pidfile, fixturePostmasterRecord(data, string(match[1]))); err != nil {
		return err
	}
	<-stopped
	if err := record("postgres-stop"); err != nil {
		return err
	}
	return os.Remove(pidfile)
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
