package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func taskHostTestCell(t *testing.T) (*Cell, ServiceDescriptor) {
	t.Helper()
	c, manifest := unitCell(t)
	manifest.InstallationID = "taskhostfixture"
	if err := writeAtomic(c.path("manifest.json"), manifest, nil); err != nil {
		t.Fatal("cannot publish task host manifest fixture")
	}
	t.Setenv("WITSELF_RUNTIME", "local")
	host, err := os.Executable()
	if err != nil {
		t.Fatal("cannot resolve task host fixture executable")
	}
	d := ServiceDescriptor{Adapter: "windows-logon-task", Payload: ServicePayload{Host: host, Args: []string{"run", "--root", c.cfg.Root}}}
	if err := c.PrepareService(t.Context(), d); err != nil {
		t.Fatal("cannot prepare task host fixture descriptor")
	}
	d, err = c.ServiceDescriptor()
	if err != nil {
		t.Fatal("cannot reopen task host fixture descriptor")
	}
	return c, d
}

func TestTaskHostValidationReopensManifestOwnerGenerationAndExecutable(t *testing.T) {
	c, original := taskHostTestCell(t)
	t.Setenv("WITSELF_RUNTIME", "hosted")
	ownerCalled := false
	owner := func(uid string) error {
		if uid != original.UID {
			t.Fatal("task host passed an unvalidated owner to the token gate")
		}
		ownerCalled = true
		return nil
	}
	d, err := c.validateTaskHost(original.Payload.Generation, owner)
	if err != nil || !reflect.DeepEqual(d, original) || !ownerCalled || os.Getenv("WITSELF_RUNTIME") != "hosted" {
		t.Fatal("read only task host validation changed runtime or descriptor")
	}
	for _, test := range []struct {
		name   string
		change func(*ServiceDescriptor)
	}{
		{"adapter", func(d *ServiceDescriptor) { d.Adapter = "foreground" }},
		{"generation", func(d *ServiceDescriptor) { d.Payload.Generation += "-different" }},
		{"owner", func(d *ServiceDescriptor) { d.UID = "different-owner" }},
		{"manifest", func(d *ServiceDescriptor) { d.InstallationID = "differentinstallation" }},
		{"host", func(d *ServiceDescriptor) { d.Payload.Host = filepath.Join(c.cfg.Root, "missing-host") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := original
			test.change(&changed)
			if err := writeAtomic(c.path("service", "descriptor.json"), changed, nil); err != nil {
				t.Fatal("cannot change task host refusal fixture")
			}
			if _, err := c.validateTaskHost(original.Payload.Generation, owner); err == nil {
				t.Fatal("task host validation accepted a mismatched identity")
			}
			if os.Getenv("WITSELF_RUNTIME") != "hosted" {
				t.Fatal("refused task host validation selected local runtime")
			}
		})
	}
	if err := writeAtomic(c.path("service", "descriptor.json"), original, nil); err != nil {
		t.Fatal("cannot restore task host validation fixture")
	}
	if _, err := c.ValidateTaskHost(""); !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("task host validation accepted an empty generation")
	}
}

func TestTaskHostPublicEntryUsesOwnerValidation(t *testing.T) {
	c, descriptor := taskHostTestCell(t)
	_, err := c.ValidateTaskHost(descriptor.Payload.Generation)
	ownerErr := validateTaskHostOwner(descriptor.UID)
	if ownerErr == nil && err != nil || ownerErr != nil && !errors.Is(err, ErrServiceManagerUnavailable) {
		t.Fatal("task host public entry disagreed with native owner policy")
	}
	refusal := errors.New("fixture token refusal")
	if _, err := c.validateTaskHost(descriptor.Payload.Generation, func(string) error { return refusal }); !errors.Is(err, refusal) {
		t.Fatal("task host ignored its native owner validation refusal")
	}
	source, err := os.ReadFile("service_task_host.go")
	if err != nil {
		t.Fatal("cannot read task host validation contract")
	}
	normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
	if !strings.Contains(normalized, "func (c *Cell) ValidateTaskHost(generation string) (ServiceDescriptor, error) {\n\treturn c.validateTaskHost(generation, validateTaskHostOwner)\n}") {
		t.Fatal("task host public entry omitted the native owner gate")
	}
}

func TestTaskHostValidationRefusesMissingDescriptorWithoutAdoption(t *testing.T) {
	c, _ := unitCell(t)
	t.Setenv("WITSELF_RUNTIME", "hosted")
	if _, err := c.ValidateTaskHost("generation-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("task host validation accepted an unprepared installation")
	}
	if _, err := os.Lstat(c.path("service")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("task host validation prepared missing service state")
	}
}
