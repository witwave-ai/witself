package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func shutdownDescriptorFixture(t *testing.T) serviceShutdownDescriptor {
	t.Helper()
	pipe, err := newServiceControlPipeName("lc_shutdown_fixture")
	if err != nil {
		t.Fatal("fixture pipe generation failed")
	}
	nonce, err := shutdownRandom(16)
	if err != nil {
		t.Fatal("fixture nonce generation failed")
	}
	auth, err := shutdownRandom(32)
	if err != nil {
		t.Fatal("fixture authentication generation failed")
	}
	return serviceShutdownDescriptor{Version: 1, InstallationID: "lc_shutdown_fixture", Generation: "generation-one", Component: "server", BootID: "boot-one", PID: 200, StartIdentity: "creation-one", PipeName: pipe + "-child-server", Nonce: nonce, Authentication: auth}
}

func TestLocalShutdownDescriptorRoundTripAndRefusals(t *testing.T) {
	good := shutdownDescriptorFixture(t)
	data, err := json.Marshal(good)
	if err != nil {
		t.Fatal("cannot encode fixture")
	}
	got, err := decodeShutdownDescriptor(bytes.NewReader(data))
	if err != nil || got != good {
		t.Fatal("valid descriptor did not round trip")
	}
	for _, row := range []struct {
		name   string
		mutate func(*serviceShutdownDescriptor)
	}{
		{"version", func(d *serviceShutdownDescriptor) { d.Version++ }},
		{"installation", func(d *serviceShutdownDescriptor) { d.InstallationID = "invalid/owner" }},
		{"generation", func(d *serviceShutdownDescriptor) { d.Generation = "" }},
		{"generation newline", func(d *serviceShutdownDescriptor) { d.Generation += "\n" }},
		{"role", func(d *serviceShutdownDescriptor) { d.Component = "postgres" }},
		{"boot", func(d *serviceShutdownDescriptor) { d.BootID = "" }},
		{"creation", func(d *serviceShutdownDescriptor) { d.StartIdentity = "" }},
		{"pid", func(d *serviceShutdownDescriptor) { d.PID = 0 }},
		{"pipe", func(d *serviceShutdownDescriptor) { d.PipeName += "extra" }},
		{"pipe role", func(d *serviceShutdownDescriptor) { d.PipeName = strings.TrimSuffix(d.PipeName, "server") + "worker" }},
		{"pipe namespace", func(d *serviceShutdownDescriptor) {
			d.PipeName = strings.Replace(d.PipeName, d.InstallationID, "other", 1)
		}},
		{"nonce", func(d *serviceShutdownDescriptor) { d.Nonce = "short" }},
		{"nonce encoding", func(d *serviceShutdownDescriptor) { d.Nonce += "=" }},
		{"authentication", func(d *serviceShutdownDescriptor) { d.Authentication = "short" }},
		{"authentication encoding", func(d *serviceShutdownDescriptor) { d.Authentication += "=" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			d := good
			if !validShutdownDescriptor(d) {
				t.Fatal("clean descriptor refused before mutation")
			}
			row.mutate(&d)
			if validShutdownDescriptor(d) {
				t.Fatal("invalid descriptor accepted at its validation site")
			}
			raw, _ := json.Marshal(d)
			if _, err := decodeShutdownDescriptor(bytes.NewReader(raw)); !errors.Is(err, errLocalShutdownRefused) || err.Error() != "service_shutdown_refused: shutdown:descriptor" {
				t.Fatal("invalid descriptor did not refuse at descriptor boundary")
			}
		})
	}
	for _, bad := range [][]byte{append(append([]byte(nil), data...), data...), append(data, bytes.Repeat([]byte(" "), 4097)...), []byte(strings.Replace(string(data), "\"version\"", "\"unknown\"", 1))} {
		if _, err := decodeShutdownDescriptor(bytes.NewReader(bad)); err == nil {
			t.Fatal("descriptor framing bypassed")
		}
	}
}

func TestLocalShutdownRequestIncarnationAuthentication(t *testing.T) {
	d := shutdownDescriptorFixture(t)
	id, err := shutdownRandom(16)
	if err != nil {
		t.Fatal("cannot generate request fixture")
	}
	request := d.request("cancel", id)
	if !d.accepts(request) || !d.accepts(d.request("release", id)) {
		t.Fatal("valid request refused")
	}
	for _, change := range []func(*serviceShutdownRequest){
		func(r *serviceShutdownRequest) { r.Action = "stop" }, func(r *serviceShutdownRequest) { r.InstallationID += "x" },
		func(r *serviceShutdownRequest) { r.Generation += "x" }, func(r *serviceShutdownRequest) { r.Component = "worker" },
		func(r *serviceShutdownRequest) { r.BootID += "x" }, func(r *serviceShutdownRequest) { r.PID++ },
		func(r *serviceShutdownRequest) { r.StartIdentity += "x" }, func(r *serviceShutdownRequest) { r.Nonce += "x" },
		func(r *serviceShutdownRequest) { r.Authentication += "x" }, func(r *serviceShutdownRequest) { r.RequestID = "short" },
		func(r *serviceShutdownRequest) { r.RequestID += "=" },
	} {
		bad := request
		change(&bad)
		if d.accepts(bad) {
			t.Fatal("request identity or authentication bypassed")
		}
	}
}

func TestServiceShutdownDescriptorStrictDecode(t *testing.T) {
	descriptor := shutdownDescriptorFixture(t)
	body, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal("cannot marshal descriptor fixture")
	}
	boundary := string(body) + strings.Repeat(" ", 4096-len(body))
	got, err := decodeShutdownDescriptor(strings.NewReader(boundary))
	if err != nil || got != descriptor {
		t.Fatal("bounded shutdown descriptor did not round trip")
	}
	for _, tc := range []struct{ name, body string }{
		{"empty", ""}, {"malformed", "{"}, {"trailing document", string(body) + "{}"},
		{"unknown field", strings.TrimSuffix(string(body), "}") + `,"unexpected":"private-marker"}`},
		{"oversized", boundary + " "}, {"oversized trailing", boundary + "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeShutdownDescriptor(strings.NewReader(tc.body))
			if !errors.Is(err, errLocalShutdownRefused) || err.Error() != "service_shutdown_refused: shutdown:descriptor" || got != (serviceShutdownDescriptor{}) {
				t.Fatal("descriptor refusal leaked partial data or lost its raising site")
			}
		})
	}
}

func TestServiceShutdownDescriptorIncarnationBinding(t *testing.T) {
	d := shutdownDescriptorFixture(t)
	clean := ProcessRecord{InstallationID: d.InstallationID, Generation: d.Generation, Component: d.Component,
		PID: d.PID, StartTime: d.StartIdentity, BootID: d.BootID, ShutdownEndpoint: d.PipeName}
	for _, tc := range []struct {
		name   string
		mutate func(*ProcessRecord)
	}{
		{"installation", func(r *ProcessRecord) { r.InstallationID += "other" }},
		{"generation", func(r *ProcessRecord) { r.Generation += "other" }},
		{"role", func(r *ProcessRecord) { r.Component = "worker" }},
		{"pid", func(r *ProcessRecord) { r.PID++ }},
		{"creation", func(r *ProcessRecord) { r.StartTime += "other" }},
		{"boot", func(r *ProcessRecord) { r.BootID += "other" }},
		{"pipe", func(r *ProcessRecord) { r.ShutdownEndpoint += "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !shutdownDescriptorMatches(d, clean) {
				t.Fatal("clean descriptor and record did not match")
			}
			r := clean
			tc.mutate(&r)
			if shutdownDescriptorMatches(d, r) {
				t.Fatal("changed incarnation matched at descriptor fence")
			}
		})
	}
}

func TestServiceShutdownDescriptorPath(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "service", "children", "worker.json")
	if !shutdownDescriptorPath(root, good) {
		t.Fatal("valid private descriptor location refused")
	}
	for _, path := range []string{"worker.json", filepath.Join(root, "worker.json"), filepath.Join(root, "service", "worker.json"), filepath.Join(root, "service", "children", "worker.txt"), good + "\n", filepath.Join(root, "service", "children", "nested", "worker.json")} {
		if shutdownDescriptorPath(root, path) {
			t.Fatal("descriptor escaped the private children directory")
		}
	}
}

func TestServiceShutdownAbsentConfiguration(t *testing.T) {
	// Exercise the actual platform installer. An absent selector does not inspect
	// a descriptor, validate a role, invoke cancellation, or create a listener.
	channel, err := InstallLocalShutdown(context.Background(), func() { t.Error("absent shutdown canceled its caller") }, runtimeconfig.LocalShutdown{}, "")
	if err != nil || channel != nil {
		t.Fatal("absent local shutdown configuration installed a channel")
	}
}
