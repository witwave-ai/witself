package localcell

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestServiceWindowsOldBootRecordPermutations(t *testing.T) {
	const oldBoot = "windows:11111111-1111-1111-1111-111111111111"
	const newBoot = "windows:22222222-2222-2222-2222-222222222222"
	for _, component := range []string{"postgres", "server", "worker"} {
		for _, kind := range []string{"matching", "missing_boot", "new_boot", "pending", "live", "foreign_root", "foreign_component"} {
			t.Run(component+"/"+kind, func(t *testing.T) {
				c, _ := descriptorTestCell(t)
				if err := privateDir(c.path("pg"), true); err != nil {
					t.Fatal("fixture directory unavailable")
				}
				pid := os.Getpid()
				before, alive, err := processIdentity(pid)
				if err != nil || !alive {
					t.Fatal("bystander unavailable")
				}
				for _, name := range []string{"postgres", "server", "worker"} {
					r := ProcessRecord{PID: pid, StartTime: "windows:0:1", BootID: oldBoot, Root: c.cfg.Root, Component: name}
					if name == component {
						switch kind {
						case "missing_boot":
							r.BootID = ""
						case "new_boot":
							r.BootID = newBoot
						case "pending":
							r.LaunchPending = true
						case "live":
							r.StartTime = before
						case "foreign_root":
							r.Root += "-foreign"
						case "foreign_component":
							r.Component = "foreign"
						}
					}
					if err := writeAtomic(c.path("run", name+".json"), r, nil); err != nil {
						t.Fatal("fixture record unavailable")
					}
				}
				pidfile := []byte(fmt.Sprintf("%d\n%s\n1\n", pid, c.path("pg")))
				if err := writeAtomicBytes(c.path("pg", "postmaster.pid"), pidfile, nil); err != nil {
					t.Fatal("fixture pidfile unavailable")
				}
				err = c.reconcileOldBoot(oldBoot, newBoot)
				if kind == "matching" && err != nil {
					t.Fatal("matching old-boot records refused")
				}
				if kind != "matching" && !errors.Is(err, ErrServiceOwnershipConflict) {
					t.Fatal("unproven record authorized old-boot cleanup")
				}
				for _, path := range []string{c.path("run", "postgres.json"), c.path("run", "server.json"), c.path("run", "worker.json"), c.path("pg", "postmaster.pid")} {
					_, statErr := os.Lstat(path)
					if kind == "matching" && !errors.Is(statErr, os.ErrNotExist) {
						t.Fatal("proven old record retained")
					}
					if kind != "matching" && statErr != nil {
						t.Fatal("refusal partially removed old records")
					}
				}
				after, alive, err := processIdentity(pid)
				if err != nil || !alive || before != after {
					t.Fatal("old-boot cleanup signaled the bystander")
				}
			})
		}
	}
}

func TestServiceProcessBootBindingExplicitLocalOnly(t *testing.T) {
	c, _ := descriptorTestCell(t)
	identity, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("fixture identity unavailable")
	}
	const boot = "windows:11111111-1111-1111-1111-111111111111"
	for _, selection := range []string{"", "hosted", "local"} {
		t.Run("runtime_"+selection, func(t *testing.T) {
			t.Setenv("WITSELF_RUNTIME", selection)
			r := ProcessRecord{PID: os.Getpid(), StartTime: identity, Root: c.cfg.Root, Component: "server"}
			if err := writeAtomic(c.path("run", "server.json"), r, nil); err != nil {
				t.Fatal("fixture record unavailable")
			}
			queries := 0
			err := c.bindServiceProcessBootWithReader("server", func() (string, error) { queries++; return boot, nil })
			if err != nil {
				t.Fatal("explicit binding failed")
			}
			var got ProcessRecord
			if readJSON(c.path("run", "server.json"), &got) != nil {
				t.Fatal("bound record unavailable")
			}
			if selection == "local" {
				if queries != 1 || got.BootID != boot {
					t.Fatal("local service launch omitted boot binding")
				}
			} else if queries != 0 || got.BootID != "" {
				t.Fatal("hosted selection queried or recorded boot identity")
			}
			if got.PID != r.PID || got.StartTime != r.StartTime || got.Root != r.Root || got.Component != r.Component || got.LaunchPending {
				t.Fatal("boot binding changed the process fence")
			}
		})
	}
	other, _ := unitCell(t)
	t.Setenv("WITSELF_RUNTIME", "local")
	if err := other.bindServiceProcessBootWithReader("server", func() (string, error) { t.Fatal("descriptor-free launch queried boot identity"); return "", nil }); err != nil {
		t.Fatal("ordinary local launch required a service descriptor")
	}
}

func TestServiceProcessBootBindingRefusesUnprovenRecord(t *testing.T) {
	c, _ := descriptorTestCell(t)
	identity, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("fixture identity unavailable")
	}
	const boot = "windows:11111111-1111-1111-1111-111111111111"
	for _, kind := range []string{"wrong_start", "wrong_boot", "pending", "foreign_root", "foreign_component", "query_failure"} {
		t.Run(kind, func(t *testing.T) {
			r := ProcessRecord{PID: os.Getpid(), StartTime: identity, Root: c.cfg.Root, Component: "server"}
			switch kind {
			case "wrong_start":
				r.StartTime = "windows:0:1"
			case "wrong_boot":
				r.BootID = "different-boot"
			case "pending":
				r.LaunchPending = true
			case "foreign_root":
				r.Root += "-foreign"
			case "foreign_component":
				r.Component = "worker"
			}
			if err := writeAtomic(c.path("run", "server.json"), r, nil); err != nil {
				t.Fatal("fixture record unavailable")
			}
			reader := func() (string, error) {
				if kind == "query_failure" {
					return "", errors.New("service_boot_identity_unavailable")
				}
				return boot, nil
			}
			if c.bindServiceProcessBootWithReader("server", reader) == nil {
				t.Fatal("unproven launch acquired boot authority")
			}
			var got ProcessRecord
			if readJSON(c.path("run", "server.json"), &got) != nil || got != r {
				t.Fatal("refused binding mutated process ownership")
			}
		})
	}
}
