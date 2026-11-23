package localcell

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

func serviceOwner() (string, error) {
	u, err := user.Current()
	if err != nil || !validServiceOwner(u.Uid, os.Geteuid()) {
		return "", errors.New("service_owner_unavailable")
	}
	return u.Uid, nil
}

func validServiceOwner(uid string, effectiveUID int) bool {
	return uid != "" && uid != "0" && (effectiveUID < 0 || (effectiveUID != 0 && uid == strconv.Itoa(effectiveUID)))
}

func validateServiceHost(d ServiceDescriptor) error {
	current, err := os.Executable()
	if err != nil {
		return ErrServiceOwnershipConflict
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return ErrServiceOwnershipConflict
	}
	expected, err := filepath.EvalSymlinks(d.Payload.Host)
	if err != nil || current != expected {
		return ErrServiceOwnershipConflict
	}
	return validateServiceHostIdentity(expected)
}

func validateServiceDescriptor(d ServiceDescriptor) error {
	uid, err := serviceOwner()
	if err != nil {
		return err
	}
	return validateServiceDescriptorOwner(d, uid)
}

func validateServiceDescriptorOwner(d ServiceDescriptor, owner string) error {
	if d.Version != 1 || d.InstallationID == "" || d.UID != owner || !filepath.IsAbs(d.Root) || filepath.Clean(d.Root) != d.Root ||
		d.Scope != "user" || d.Runtime != "local" || d.Revision == 0 || d.Payload.Generation == "" || !filepath.IsAbs(d.Payload.Host) ||
		(d.Adapter != "foreground" && d.Adapter != "systemd-user" && d.Adapter != "launchd-agent" && d.Adapter != "windows-logon-task") || !validServiceDesiredState(d.DesiredState) || d.ResourceProfile != "unconfigured" {
		return ErrServiceOwnershipConflict
	}
	for _, arg := range append([]string{d.Payload.Host, d.Payload.Generation, d.InstallationID}, d.Payload.Args...) {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return ErrServiceOwnershipConflict
		}
	}
	return nil
}

func validServiceDesiredState(state string) bool {
	return state == "running" || state == "stopped" || state == "maintenance"
}

func servicePrivateRecord(path string) error {
	i, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	}
	if err != nil || !i.Mode().IsRegular() || !privatePassword(path, i) {
		return errors.New("service_record_not_private")
	}
	return nil
}

// openServiceRecord is servicePrivateRecord decided on the handle openRegularBy
// opened without waiting; a private file that cannot be opened returns open.
func openServiceRecord(ctx context.Context, path string, open error) (*os.File, error) {
	notPrivate := errors.New("service_record_not_private")
	return openRegularBy(ctx, openPrivateReader, func(info os.FileInfo) bool { return privatePassword(path, info) }, path, notPrivate, notPrivate, open)
}

// ServiceDescriptor reads the current fence and revalidates installation identity.
func (c *Cell) ServiceDescriptor() (ServiceDescriptor, error) {
	return c.serviceDescriptor(context.Background())
}

func serviceLeaseCut(err error) bool {
	var cut *leaseWaitCut
	return errors.As(err, &cut)
}

// Keep the full cause and its site when a lease wait was interrupted.
func serviceReadFailure(err, refusal error) error {
	if serviceLeaseCut(err) {
		return err
	}
	return refusal
}

func (c *Cell) serviceDescriptor(ctx context.Context) (ServiceDescriptor, error) {
	var d ServiceDescriptor
	if err := c.validateRoot(false); err != nil {
		return d, err
	}
	return c.readServiceDescriptor(ctx)
}

func (c *Cell) serviceDescriptorForOwner(ctx context.Context, owner string) (ServiceDescriptor, error) {
	var d ServiceDescriptor
	if err := c.validateRoot(false); err != nil {
		return d, err
	}
	return c.readServiceDescriptorForOwner(ctx, owner)
}

// readServiceDescriptor requires a root validated under an operation lock
// that the caller still holds.
func (c *Cell) readServiceDescriptor(ctx context.Context) (ServiceDescriptor, error) {
	return c.readServiceDescriptorForOwner(ctx, "")
}

func (c *Cell) readServiceDescriptorForOwner(ctx context.Context, owner string) (ServiceDescriptor, error) {
	var d ServiceDescriptor
	if _, err := os.Lstat(c.path("service")); errors.Is(err, os.ErrNotExist) {
		return d, os.ErrNotExist
	}
	if err := privateDir(c.path("service"), false); err != nil {
		return d, err
	}
	path := c.path("service", "descriptor.json")
	if err := readServiceRecordJSON(ctx, path, &d); err != nil {
		return d, err
	}
	if owner == "" {
		if err := validateServiceDescriptor(d); err != nil {
			return d, err
		}
	} else if err := validateServiceDescriptorOwner(d, owner); err != nil {
		return d, err
	}
	m, err := readManifestBy(ctx, c.cfg.Root, readManifestJSON)
	if err != nil {
		return d, err
	}
	if d.Root != c.cfg.Root || d.InstallationID != m.InstallationID || !m.Installed {
		return d, ErrServiceOwnershipConflict
	}
	return d, nil
}

// PrepareService publishes an opt-in descriptor and a private random control
// secret. Repeated preparation must name the same immutable payload generation.
func (c *Cell) PrepareService(ctx context.Context, d ServiceDescriptor) error {
	if os.Getenv("WITSELF_RUNTIME") != "local" {
		return errors.New("service_local_runtime_required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := c.own(ctx, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	m, err := readManifest(ctx, c.cfg.Root)
	if err != nil || !m.Installed || m.RemovalRetained {
		return serviceReadFailure(err, errors.New("service_installation_required"))
	}
	uid, err := serviceOwner()
	if err != nil {
		return err
	}
	d.Version, d.InstallationID, d.UID, d.Root = 1, m.InstallationID, uid, c.cfg.Root
	if d.Runtime == "" {
		d.Runtime = "local"
	}
	if d.Scope == "" {
		d.Scope = "user"
	}
	if d.DesiredState == "" {
		d.DesiredState = "running"
	}
	if d.ResourceProfile == "" {
		d.ResourceProfile = "unconfigured"
	}
	if d.Payload.Generation == "" {
		d.Payload.Generation = m.InstallationID + "-1"
	}
	d.Revision, d.RepairRevision, d.Faulted = 1, 0, false
	if err := validateServiceDescriptor(d); err != nil {
		return err
	}
	old, err := c.serviceDescriptor(ctx)
	if err == nil {
		if old.Adapter != d.Adapter || old.Runtime != d.Runtime || !reflect.DeepEqual(old.Payload, d.Payload) {
			return ErrServiceOwnershipConflict
		}
		_, err = c.serviceControlSecret(ctx)
		return err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := c.maintenanceFenceAllows(ctx, 0); err != nil {
		return err
	}
	if err := privateDir(c.path("service"), true); err != nil {
		return err
	}
	if _, err := c.serviceControlSecret(ctx); errors.Is(err, os.ErrNotExist) {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return errors.New("service_control_secret_failed")
		}
		if err := writeAtomicBytes(c.path("service", "control.secret"), []byte(base64.RawURLEncoding.EncodeToString(secret[:])), nil); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return c.writeServiceDescriptor(ctx, d)
}

func (c *Cell) serviceControlSecret(ctx context.Context) (string, error) {
	return c.serviceControlSecretBy(ctx, openServiceRecord)
}

func (c *Cell) serviceControlSecretBy(ctx context.Context, open func(context.Context, string, error) (*os.File, error)) (string, error) {
	if err := privateDir(c.path("service"), false); err != nil {
		return "", err
	}
	invalid := errors.New("service_control_secret_invalid")
	f, err := open(ctx, c.path("service", "control.secret"), invalid)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil || len(b) != 43 {
		return "", invalid
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(string(b)); err != nil || len(decoded) != 32 {
		return "", errors.New("service_control_secret_invalid")
	}
	return string(b), nil
}

func (c *Cell) serviceDescriptorBoundary(stage string, d ServiceDescriptor) {
	if c.observeServiceDescriptor != nil {
		c.observeServiceDescriptor(stage, d)
	}
}

// Descriptor publication is an acknowledged journaled transition. The pending
// record is replayed conservatively before any launch; it cannot relax a fence.
func (c *Cell) writeServiceDescriptor(_ context.Context, d ServiceDescriptor) (result error) {
	defer func() { result = serviceFailureFallback(result, "descriptor_write") }()
	if err := writeAtomic(c.path("service", "transition.json"), d, nil); err != nil {
		return err
	}
	c.serviceDescriptorBoundary("journal", d)
	if err := writeAtomic(c.path("service", "descriptor.json"), d, nil); err != nil {
		return err
	}
	c.serviceDescriptorBoundary("descriptor", d)
	if err := removePrivateFile(c.path("service", "transition.json")); err != nil {
		return err
	}
	if err := syncDirectory(c.path("service")); err != nil {
		return err
	}
	c.serviceDescriptorBoundary("retired", d)
	return nil
}

func (c *Cell) serviceStartFence(ctx context.Context) error {
	d, err := c.serviceDescriptor(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := os.Lstat(c.path("service", "transition.json")); !errors.Is(err, os.ErrNotExist) {
		return ErrServiceDesiredState
	}
	if d.DesiredState != "running" || d.Faulted {
		return ErrServiceDesiredState
	}
	return nil
}

// SetServiceDesiredState durably fences all future launches before returning.
func (c *Cell) SetServiceDesiredState(ctx context.Context, state string) error {
	if !validServiceDesiredState(state) {
		return ErrServiceDesiredState
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := c.own(ctx, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	return c.setServiceDesiredStateLocked(ctx, state)
}

func (c *Cell) setServiceDesiredStateLocked(ctx context.Context, state string) error {
	d, err := c.serviceDescriptor(ctx)
	if err != nil {
		return err
	}
	if d.Faulted && state == "running" {
		return ErrServiceDesiredState
	}
	if state == "running" {
		if d.DesiredState == "maintenance" {
			return ErrServiceDesiredState
		}
		if _, err := os.Lstat(c.path("service", "transition.json")); !errors.Is(err, os.ErrNotExist) {
			return ErrServiceDesiredState
		}
		if err := c.maintenanceFenceAllows(ctx, 0); err != nil {
			return err
		}
	}
	d.DesiredState, d.Revision = state, d.Revision+1
	return c.writeServiceDescriptor(ctx, d)
}

func (c *Cell) setServiceFaulted(ctx context.Context, faulted bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.commitServiceFault(ctx, faulted, nil)
}

// A receipt belongs to one refused stop on one live supervisor. Only a fully
// retired descriptor transition can issue it.
type serviceFaultCommit struct {
	installationID string
	generation     string
	revision       uint64
}

func (c *Cell) commitServiceFault(ctx context.Context, faulted bool, prior *serviceFaultCommit) error {
	lock, err := c.own(ctx, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	d, err := c.serviceDescriptor(ctx)
	if err != nil {
		return err
	}
	if prior != nil {
		if faulted && d.Faulted && prior.revision == d.Revision &&
			prior.installationID == d.InstallationID && prior.generation == d.Payload.Generation {
			_, err := os.Lstat(c.path("service", "transition.json"))
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
		}
		// The old receipt cannot survive an interrupted replacement.
		*prior = serviceFaultCommit{}
	}
	d.Faulted, d.Revision = faulted, d.Revision+1
	if err := c.writeServiceDescriptor(ctx, d); err != nil {
		return err
	}
	if prior != nil {
		*prior = serviceFaultCommit{installationID: d.InstallationID, generation: d.Payload.Generation, revision: d.Revision}
	}
	return nil
}

// RepairService explicitly acknowledges the fault and converges to stopped.
// Start is a separate authenticated action; maintenance is never auto-cleared.
func (c *Cell) RepairService(ctx context.Context) error {
	if err := c.SetServiceDesiredState(ctx, "stopped"); err != nil {
		return err
	}
	if err := c.Repair(ctx); err != nil {
		return err
	}
	if err := c.stopServiceChildren(ctx); err != nil {
		return err
	}
	lock, err := c.own(ctx, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	d, err := c.serviceDescriptor(ctx)
	if err != nil {
		return err
	}
	if err := c.maintenanceFenceAllows(ctx, 0); err != nil {
		return err
	}
	d.Faulted, d.Revision, d.RepairRevision = false, d.Revision+1, d.RepairRevision+1
	return c.writeServiceDescriptor(ctx, d)
}
