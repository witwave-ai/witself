package localcell

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// The endpoint is discovery data, never authentication authority. Pipe names
// are intentionally absent from addresses, errors and diagnostics.
type serviceControlEndpointRecord struct {
	Version            int    `json:"version"`
	InstallationID     string `json:"installation_id"`
	Generation         string `json:"generation"`
	DescriptorRevision uint64 `json:"descriptor_revision"`
	BootID             string `json:"boot_id"`
	HostPID            int    `json:"host_pid"`
	HostStartIdentity  string `json:"host_start_identity"`
	PipeName           string `json:"pipe_name"`
}

const serviceControlPipePrefix = `\\.\pipe\witself-cell-`

func serviceEndpointRefused(step string) error {
	return &serviceControlRefusal{step: "endpoint:" + step}
}

// Only fixed internal steps enter this type. It lets Control retain a refusal
// through net/http's URL wrapper without returning the URL or raw API errors.
type serviceControlRefusal struct{ step string }

func (e *serviceControlRefusal) Error() string {
	return ErrServiceControlRefused.Error() + ": " + e.step
}
func (e *serviceControlRefusal) Unwrap() error { return ErrServiceControlRefused }

func validServicePipeInstallation(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func newServiceControlPipeName(installationID string) (string, error) {
	if !validServicePipeInstallation(installationID) {
		return "", serviceEndpointRefused("record")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", serviceEndpointRefused("random")
	}
	return serviceControlPipePrefix + installationID + "-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(nonce[:])), nil
}

func validServiceControlPipeName(name, installationID string) bool {
	prefix := serviceControlPipePrefix + installationID + "-"
	if !validServicePipeInstallation(installationID) || len(name) >= 256 || !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if len(suffix) != 26 || suffix != strings.ToLower(suffix) {
		return false
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(suffix))
	return err == nil && len(decoded) == 16 && strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(decoded)) == suffix
}

func decodeServiceControlEndpoint(r io.Reader) (serviceControlEndpointRecord, error) {
	var record serviceControlEndpointRecord
	// Read one byte past the cap so whitespace cannot hide a trailing object
	// or unknown fields beyond a synthetic EOF at the boundary.
	limited := &io.LimitedReader{R: r, N: 4097}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || limited.N == 0 {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	return record, nil
}

func validateServiceControlRecord(record serviceControlEndpointRecord, d ServiceDescriptor, lease serviceLeaseRecord, boot string) error {
	if record.Version != 1 || record.InstallationID != d.InstallationID || record.Generation != d.Payload.Generation ||
		record.DescriptorRevision != d.Revision || boot == "" || record.BootID != boot ||
		!validServiceControlPipeName(record.PipeName, d.InstallationID) {
		return serviceEndpointRefused("record")
	}
	if record.HostPID <= 1 || record.HostStartIdentity == "" || lease.Version != 1 || lease.PID != record.HostPID ||
		lease.StartIdentity != record.HostStartIdentity || lease.BootID != record.BootID || lease.InstallationID != record.InstallationID || lease.Owner != d.UID {
		return serviceEndpointRefused("host_identity")
	}
	return nil
}

func (c *Cell) readServiceControlEndpointRecord() (endpoint serviceControlEndpointRecord, result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_read") }()
	if privateDir(c.path("service"), false) != nil {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	path := c.path("service", "control-endpoint.json")
	if servicePrivateRecord(path) != nil {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	f, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	defer func() { _ = f.Close() }()
	return decodeServiceControlEndpoint(f)
}

func (c *Cell) readServiceControlEndpoint() (serviceControlEndpointRecord, error) {
	return c.readServiceControlEndpointWithBoot(serviceBootID)
}

func (c *Cell) readServiceControlEndpointWithBoot(readBoot func() (string, error)) (serviceControlEndpointRecord, error) {
	d, err := c.ServiceDescriptor()
	if err != nil {
		return serviceControlEndpointRecord{}, serviceFailureAt(serviceEndpointRefused("record"), "endpoint_descriptor")
	}
	record, err := c.readServiceControlEndpointRecord()
	if err != nil {
		return serviceControlEndpointRecord{}, err
	}
	var lease serviceLeaseRecord
	if servicePrivateRecord(c.path("service", "lease.json")) != nil || readJSON(c.path("service", "lease.json"), &lease) != nil {
		return serviceControlEndpointRecord{}, serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_lease")
	}
	boot, err := readBoot()
	if err != nil {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	if err := validateServiceControlRecord(record, d, lease, boot); err != nil {
		return serviceControlEndpointRecord{}, err
	}
	return record, nil
}

// Called only after the Windows listener has reserved its first instance.
func (c *Cell) publishServiceControlEndpoint(pipeName string) (serviceControlEndpointRecord, error) {
	return c.publishServiceControlEndpointWithBoot(pipeName, serviceBootID)
}

func (c *Cell) publishServiceControlEndpointWithBoot(pipeName string, readBoot func() (string, error)) (endpoint serviceControlEndpointRecord, result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_write") }()
	d, err := c.ServiceDescriptor()
	if err != nil {
		return serviceControlEndpointRecord{}, serviceFailureAt(serviceEndpointRefused("record"), "endpoint_descriptor")
	}
	var lease serviceLeaseRecord
	if readJSON(c.path("service", "lease.json"), &lease) != nil || servicePrivateRecord(c.path("service", "lease.json")) != nil {
		return serviceControlEndpointRecord{}, serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_lease")
	}
	boot, err := readBoot()
	if err != nil {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	start, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("host_identity")
	}
	record := serviceControlEndpointRecord{Version: 1, InstallationID: d.InstallationID, Generation: d.Payload.Generation,
		DescriptorRevision: d.Revision, BootID: boot, HostPID: os.Getpid(), HostStartIdentity: start, PipeName: pipeName}
	if err := validateServiceControlRecord(record, d, lease, boot); err != nil {
		return serviceControlEndpointRecord{}, err
	}
	if err := writeAtomic(c.path("service", "control-endpoint.json"), record, nil); err != nil {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	return record, nil
}

func sameServiceControlHost(a, b serviceControlEndpointRecord) bool {
	return a.HostPID == b.HostPID && a.HostStartIdentity == b.HostStartIdentity && a.PipeName == b.PipeName
}

func (c *Cell) removeOwnedServiceControlEndpoint(owned serviceControlEndpointRecord) (result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_remove") }()
	record, err := c.readServiceControlEndpointRecord()
	if err != nil {
		return serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_read")
	}
	if !sameServiceControlHost(record, owned) {
		return serviceEndpointRefused("host_identity")
	}
	if err := removePrivateFile(c.path("service", "control-endpoint.json")); err != nil {
		return serviceEndpointRefused("record")
	}
	return nil
}

// An authenticated command may advance the descriptor revision. Only the
// listener-owning Cell can advance its endpoint, and a replacement is preserved.
// Unix has no endpoint state, so this introduces no filesystem or API operation.
func (c *Cell) refreshOwnedServiceControlEndpoint(d ServiceDescriptor) (result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_write") }()
	c.serviceControlMu.Lock()
	defer c.serviceControlMu.Unlock()
	if c.serviceControlOwned == nil {
		return nil
	}
	old, err := c.readServiceControlEndpointRecord()
	if err != nil {
		return serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_read")
	}
	if !sameServiceControlHost(old, *c.serviceControlOwned) || old.InstallationID != d.InstallationID || old.Generation != d.Payload.Generation {
		return serviceEndpointRefused("host_identity")
	}
	old.DescriptorRevision = d.Revision
	if writeAtomic(c.path("service", "control-endpoint.json"), old, nil) != nil {
		return serviceEndpointRefused("record")
	}
	c.serviceControlOwned = &old
	return nil
}
