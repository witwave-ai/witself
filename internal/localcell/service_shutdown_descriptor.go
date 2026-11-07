package localcell

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
)

// LocalShutdownChannel is installed only for an explicit local child launch.
// WaitRelease fences all store work; RuntimeReady makes an accepted stop eligible
// for bounded escalation. Close belongs after the application's drain completes.
type LocalShutdownChannel interface {
	WaitRelease(context.Context) error
	RuntimeReady() error
	Close() error
}

var errLocalShutdownRefused = errors.New("service_shutdown_refused")

type localShutdownRefusal struct{ step string }

func (e *localShutdownRefusal) Error() string { return errLocalShutdownRefused.Error() + ": " + e.step }
func (e *localShutdownRefusal) Unwrap() error { return errLocalShutdownRefused }
func shutdownRefused(step string) error       { return &localShutdownRefusal{step: step} }

// Authentication and request nonces are private launch material. Never include
// this record, its fields, or a decoder error in diagnostics.
type serviceShutdownDescriptor struct {
	Version        int    `json:"version"`
	InstallationID string `json:"installation_id"`
	Generation     string `json:"generation"`
	Component      string `json:"component"`
	BootID         string `json:"boot_id"`
	PID            int    `json:"pid"`
	StartIdentity  string `json:"start_identity"`
	PipeName       string `json:"pipe_name"`
	Nonce          string `json:"nonce"`
	Authentication string `json:"authentication"`
}

func shutdownRandom(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", shutdownRefused("shutdown:descriptor")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func shutdownCanonicalRandom(value string, size int) bool {
	b, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(b) == size && base64.RawURLEncoding.EncodeToString(b) == value
}

func validShutdownDescriptor(d serviceShutdownDescriptor) bool {
	name := strings.TrimSuffix(d.PipeName, "-child-"+d.Component)
	return d.Version == 1 && validServicePipeInstallation(d.InstallationID) &&
		d.Generation != "" && !strings.ContainsAny(d.Generation, "\x00\r\n") &&
		(d.Component == "server" || d.Component == "worker") && d.BootID != "" &&
		d.PID > 1 && d.StartIdentity != "" && name != d.PipeName && validServiceControlPipeName(name, d.InstallationID) &&
		shutdownCanonicalRandom(d.Nonce, 16) && shutdownCanonicalRandom(d.Authentication, 32)
}

func decodeShutdownDescriptor(r io.Reader) (serviceShutdownDescriptor, error) {
	var d serviceShutdownDescriptor
	limited := &io.LimitedReader{R: r, N: 4097}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d) != nil || decoder.Decode(new(any)) != io.EOF || limited.N == 0 || !validShutdownDescriptor(d) {
		return serviceShutdownDescriptor{}, shutdownRefused("shutdown:descriptor")
	}
	return d, nil
}

func shutdownDescriptorMatches(d serviceShutdownDescriptor, record ProcessRecord) bool {
	return validShutdownDescriptor(d) && d.Component == record.Component && d.PID == record.PID &&
		d.StartIdentity == record.StartTime && d.BootID == record.BootID && d.PipeName == record.ShutdownEndpoint &&
		d.InstallationID == record.InstallationID && d.Generation == record.Generation
}

func shutdownDescriptorPath(root, path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Dir(path) == filepath.Join(root, "service", "children") &&
		strings.HasSuffix(filepath.Base(path), ".json") && !strings.ContainsAny(path, "\x00\r\n")
}

type serviceShutdownRequest struct {
	Action         string `json:"action"`
	InstallationID string `json:"installation_id"`
	Generation     string `json:"generation"`
	Component      string `json:"component"`
	BootID         string `json:"boot_id"`
	PID            int    `json:"pid"`
	StartIdentity  string `json:"start_identity"`
	RequestID      string `json:"request_id"`
	Nonce          string `json:"nonce"`
	Authentication string `json:"authentication"`
}

func (d serviceShutdownDescriptor) request(action, id string) serviceShutdownRequest {
	return serviceShutdownRequest{action, d.InstallationID, d.Generation, d.Component, d.BootID, d.PID, d.StartIdentity, id, d.Nonce, d.Authentication}
}

func (d serviceShutdownDescriptor) accepts(r serviceShutdownRequest) bool {
	return (r.Action == "release" || r.Action == "cancel") && shutdownCanonicalRandom(r.RequestID, 16) &&
		r.InstallationID == d.InstallationID && r.Generation == d.Generation && r.Component == d.Component &&
		r.BootID == d.BootID && r.PID == d.PID && r.StartIdentity == d.StartIdentity &&
		subtle.ConstantTimeCompare([]byte(r.Nonce), []byte(d.Nonce)) == 1 &&
		subtle.ConstantTimeCompare([]byte(r.Authentication), []byte(d.Authentication)) == 1
}
