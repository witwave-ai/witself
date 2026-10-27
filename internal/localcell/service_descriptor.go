package localcell

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
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
	return nil
}

func validateServiceDescriptor(d ServiceDescriptor) error {
	uid, err := serviceOwner()
	if err != nil {
		return err
	}
	if d.Version != 1 || d.InstallationID == "" || d.UID != uid || !filepath.IsAbs(d.Root) || filepath.Clean(d.Root) != d.Root ||
		d.Scope != "user" || d.Runtime != "local" || d.Revision == 0 || d.Payload.Generation == "" || !filepath.IsAbs(d.Payload.Host) ||
		(d.Adapter != "foreground" && d.Adapter != "systemd-user") || !validServiceDesiredState(d.DesiredState) || d.ResourceProfile != "unconfigured" {
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

// ServiceDescriptor reads the current fence and revalidates installation identity.
func (c *Cell) ServiceDescriptor() (ServiceDescriptor, error) {
	var d ServiceDescriptor
	if err := c.validateRoot(false); err != nil {
		return d, err
	}
	if _, err := os.Lstat(c.path("service")); errors.Is(err, os.ErrNotExist) {
		return d, os.ErrNotExist
	}
	if err := privateDir(c.path("service"), false); err != nil {
		return d, err
	}
	path := c.path("service", "descriptor.json")
	if err := servicePrivateRecord(path); err != nil {
		return d, err
	}
	if err := readJSON(path, &d); err != nil {
		return d, err
	}
	if err := validateServiceDescriptor(d); err != nil {
		return d, err
	}
	m, err := readManifest(c.cfg.Root)
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
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	m, err := readManifest(c.cfg.Root)
	if err != nil || !m.Installed {
		return errors.New("service_installation_required")
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
	old, err := c.ServiceDescriptor()
	if err == nil {
		if old.Adapter != d.Adapter || old.Runtime != d.Runtime || !reflect.DeepEqual(old.Payload, d.Payload) {
			return ErrServiceOwnershipConflict
		}
		_, err = c.serviceControlSecret()
		return err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := privateDir(c.path("service"), true); err != nil {
		return err
	}
	if _, err := c.serviceControlSecret(); errors.Is(err, os.ErrNotExist) {
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
	return c.writeServiceDescriptor(d)
}

func (c *Cell) serviceControlSecret() (string, error) {
	if err := privateDir(c.path("service"), false); err != nil {
		return "", err
	}
	path := c.path("service", "control.secret")
	if err := servicePrivateRecord(path); err != nil {
		return "", err
	}
	b, err := readPrivateFile(path)
	if err != nil || len(b) != 43 {
		return "", errors.New("service_control_secret_invalid")
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(string(b)); err != nil || len(decoded) != 32 {
		return "", errors.New("service_control_secret_invalid")
	}
	return string(b), nil
}

// Descriptor publication is an acknowledged journaled transition. The pending
// record is replayed conservatively before any launch; it cannot relax a fence.
func (c *Cell) writeServiceDescriptor(d ServiceDescriptor) error {
	if err := writeAtomic(c.path("service", "transition.json"), d, nil); err != nil {
		return err
	}
	if err := writeAtomic(c.path("service", "descriptor.json"), d, nil); err != nil {
		return err
	}
	if err := removePrivateFile(c.path("service", "transition.json")); err != nil {
		return err
	}
	return syncDirectory(c.path("service"))
}

func (c *Cell) serviceStartFence() error {
	d, err := c.ServiceDescriptor()
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
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	return c.setServiceDesiredStateLocked(state)
}

func (c *Cell) setServiceDesiredStateLocked(state string) error {
	d, err := c.ServiceDescriptor()
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
	}
	d.DesiredState, d.Revision = state, d.Revision+1
	return c.writeServiceDescriptor(d)
}

func (c *Cell) setServiceFaulted(ctx context.Context, faulted bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	d, err := c.ServiceDescriptor()
	if err != nil {
		return err
	}
	d.Faulted, d.Revision = faulted, d.Revision+1
	return c.writeServiceDescriptor(d)
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
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	d, err := c.ServiceDescriptor()
	if err != nil {
		return err
	}
	d.Faulted, d.Revision, d.RepairRevision = false, d.Revision+1, d.RepairRevision+1
	return c.writeServiceDescriptor(d)
}
