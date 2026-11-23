package localcell

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
)

// The endpoint is discovery data, never authentication authority. Pipe names
// are intentionally absent from addresses, errors and diagnostics.
type serviceControlEndpointRecord struct {
	Version           int    `json:"version"`
	InstallationID    string `json:"installation_id"`
	Generation        string `json:"generation"`
	BindingRevision   uint64 `json:"binding_revision"`
	BootID            string `json:"boot_id"`
	HostPID           int    `json:"host_pid"`
	HostStartIdentity string `json:"host_start_identity"`
	PipeName          string `json:"pipe_name"`
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
	if record.Version != 2 || record.InstallationID != d.InstallationID || record.Generation != d.Payload.Generation ||
		record.BindingRevision == 0 || record.BindingRevision > d.Revision || record.BindingRevision != lease.BindingRevision ||
		record.PipeName != lease.PipeName || record.Generation != lease.Generation || boot == "" || record.BootID != boot ||
		!validServiceControlPipeName(record.PipeName, d.InstallationID) {
		return serviceEndpointRefused("record")
	}
	if record.HostPID <= 1 || record.HostStartIdentity == "" || lease.Version != 2 || lease.PID != record.HostPID ||
		lease.StartIdentity != record.HostStartIdentity || lease.BootID != record.BootID || lease.InstallationID != record.InstallationID || lease.Owner != d.UID {
		return serviceEndpointRefused("host_identity")
	}
	return nil
}

func (c *Cell) readServiceControlEndpointRecord() (serviceControlEndpointRecord, error) {
	return c.readServiceControlEndpointRecordBy(context.Background(), openServiceRecord)
}

func (c *Cell) readServiceControlEndpointRecordBy(ctx context.Context, open func(context.Context, string, error) (*os.File, error)) (endpoint serviceControlEndpointRecord, result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_read") }()
	if privateDir(c.path("service"), false) != nil {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	f, err := open(ctx, c.path("service", "control-endpoint.json"), serviceEndpointRefused("record"))
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
	c.serviceDescriptorBoundary("sampled", d)
	record, err := c.readServiceControlEndpointRecordBy(context.Background(), openAdmissionRecord)
	if err != nil {
		return serviceControlEndpointRecord{}, err
	}
	var lease serviceLeaseRecord
	if readServiceRecordJSON(context.Background(), c.path("service", "lease.json"), &lease) != nil {
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

// Run checked its executable against initial before taking the lifetime lock.
// Sample the issue revision under that lock, without adopting a different
// installation, owner or executable payload during startup.
func (c *Cell) serviceListenerDescriptor(initial ServiceDescriptor) (ServiceDescriptor, error) {
	d, err := c.ServiceDescriptor()
	if err != nil || d.InstallationID != initial.InstallationID || d.UID != initial.UID || !reflect.DeepEqual(d.Payload, initial.Payload) {
		return ServiceDescriptor{}, serviceEndpointRefused("record")
	}
	return d, nil
}

// The first instance is already reserved. Only a provisional lease belonging
// to this activation's host may become eligible for discovery.
func (c *Cell) completeServiceLease(initial ServiceDescriptor, name string, readBoot func() (string, error)) error {
	// Reservation can overlap a state writer. Issue at the revision of a
	// current descriptor for the same validated host payload.
	d, err := c.serviceListenerDescriptor(initial)
	if err != nil {
		return serviceFailureAt(err, "endpoint_descriptor")
	}
	provisional := serviceLeaseRecord{}
	path := c.path("service", "lease.json")
	if err := readServiceRecordJSON(context.Background(), path, &provisional); err != nil {
		return serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_lease")
	}
	start, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive || provisional.PID != os.Getpid() || provisional.StartIdentity != start ||
		provisional.Version != 2 || provisional.Owner != d.UID || provisional.InstallationID != d.InstallationID ||
		provisional.Generation != "" || provisional.BindingRevision != 0 || provisional.PipeName != "" {
		return serviceEndpointRefused("host_identity")
	}
	boot, err := readBoot()
	if err != nil || boot == "" || provisional.BootID != boot || d.Revision == 0 || d.Payload.Generation == "" || !validServiceControlPipeName(name, d.InstallationID) {
		return serviceEndpointRefused("record")
	}
	provisional.PipeName = name
	provisional.BindingRevision = d.Revision
	provisional.Generation = d.Payload.Generation
	if err := c.publishServiceLease(path, provisional); err != nil {
		return serviceFailureAt(err, "lease_write")
	}
	return nil
}

func serviceEndpointForLease(lease serviceLeaseRecord) serviceControlEndpointRecord {
	return serviceControlEndpointRecord{
		Version: lease.Version, InstallationID: lease.InstallationID, Generation: lease.Generation,
		BindingRevision: lease.BindingRevision, BootID: lease.BootID, HostPID: lease.PID,
		HostStartIdentity: lease.StartIdentity, PipeName: lease.PipeName,
	}
}

// Publication revalidates the descriptor and the completed lease. An
// intervening state-only write does not change the listener's issue revision.
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
	if readServiceRecordJSON(context.Background(), c.path("service", "lease.json"), &lease) != nil {
		return serviceControlEndpointRecord{}, serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_lease")
	}
	boot, err := readBoot()
	if err != nil {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	start, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive || lease.PID != os.Getpid() || lease.StartIdentity != start {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("host_identity")
	}
	record := serviceEndpointForLease(lease)
	if record.PipeName != pipeName {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	if err := validateServiceControlRecord(record, d, lease, boot); err != nil {
		return serviceControlEndpointRecord{}, err
	}
	if err := writeAtomic(c.path("service", "control-endpoint.json"), record, nil); err != nil {
		return serviceControlEndpointRecord{}, serviceEndpointRefused("record")
	}
	return record, nil
}

func (c *Cell) removeOwnedServiceControlEndpoint(owned serviceControlEndpointRecord) error {
	return c.removeOwnedServiceControlEndpointBy(owned, nil)
}

func (c *Cell) removeOwnedServiceControlEndpointBy(owned serviceControlEndpointRecord, matched func()) (result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_remove") }()
	path := c.path("service", "control-endpoint.json")
	if privateDir(c.path("service"), false) != nil {
		return serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_read")
	}
	f, err := openServiceRecord(context.Background(), path, serviceEndpointRefused("record"))
	if err != nil {
		return serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_read")
	}
	defer func() { _ = f.Close() }()
	record, err := decodeServiceControlEndpoint(f)
	if err != nil {
		return serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_read")
	}
	if record != owned {
		return serviceEndpointRefused("host_identity")
	}
	if matched != nil {
		matched()
	}
	if err := removeServiceEndpointFile(path, f); err != nil {
		return serviceEndpointRefused("record")
	}
	return nil
}
