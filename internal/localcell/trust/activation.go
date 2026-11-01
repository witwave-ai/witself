package trust

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// ServicePayload is assignment-compatible with localcell.ServicePayload without
// importing the lifecycle package into the standard-library-only trust boundary.
// Activate is the sole producer of this shape from a verified payload generation.
type ServicePayload = struct {
	Generation string   `json:"generation"`
	Host       string   `json:"host"`
	Args       []string `json:"args"`
}

// HostIdentity pins the open executable, including content, for the launch check.
type HostIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// ActivationReceipt is private handoff evidence, never ordinary harness output.
type ActivationReceipt struct {
	Version     int          `json:"version"`
	Generation  string       `json:"generation"`
	Host        string       `json:"host"`
	Args        []string     `json:"args"`
	Identity    HostIdentity `json:"identity"`
	ActivatedAt time.Time    `json:"activated_at"`
}

// ReasonPayloadIdentity means the selected host no longer has its accepted identity.
const ReasonPayloadIdentity Reason = "payload_identity_changed"

var generationNamePattern = regexp.MustCompile(`^stable-[1-9][0-9]{0,19}-[0-9a-f]{16}$`)

func validGenerationName(name string) bool { return generationNamePattern.MatchString(name) }

func checkActivation(ctx context.Context, root, name string, clock Clock, offline bool) (ServicePayload, ActivationReceipt, error) {
	var payload ServicePayload
	var receipt ActivationReceipt
	if ctx.Err() != nil {
		return payload, receipt, ReasonStateIO
	}
	if err := checkPayloadOwnership(root); err != nil {
		return payload, receipt, err
	}
	g, err := LoadGeneration(root, name)
	if err != nil {
		return payload, receipt, err
	}
	v := generationVerifier(g, clock)
	// Stage persists the current accepted authority here. Generation.Trust is
	// historical evidence only; it cannot erase later acquisition revocations.
	stateDir := filepath.Join(root, "trust")
	if privateDirectory(stateDir, false) != nil {
		return payload, receipt, ReasonStateIO
	}
	state, err := NewFileState(stateDir)
	if err != nil {
		return payload, receipt, err
	}
	options := v.options
	options.State = state
	v = NewVerifier(options)
	if out := v.AcceptForOfflineStart(g.Document, g.Envelope); !out.Accepted {
		return payload, receipt, out.Reason
	}
	if !offline {
		if out := v.AcceptForAcquisition(g.Document, g.Envelope); !out.Accepted {
			return payload, receipt, out.Reason
		}
	}
	if err := RecheckGeneration(root, g); err != nil {
		return payload, receipt, err
	}
	host := filepath.Join(root, "payload", name, filepath.FromSlash(g.Host))
	identity, err := readHostIdentity(host)
	if err != nil {
		return payload, receipt, err
	}
	// Compare the descriptor opened after the full recheck to the signed host,
	// rather than blessing whichever bytes happen to occupy that name now.
	matched := false
	for _, component := range g.Components {
		for _, entry := range component.Component.Inventory {
			if filepath.ToSlash(filepath.Join(component.Directory, entry.Path)) == g.Host && component.Component.Role == "service_host" && entry.Executable && identity.Digest == entry.SHA256 && identity.Size == int64(entry.Size) {
				matched = true
			}
		}
	}
	if !matched {
		return payload, receipt, ReasonPayloadIdentity
	}
	payload = ServicePayload{Generation: name, Host: host, Args: append([]string(nil), g.Args...)}
	receipt = ActivationReceipt{Version: 1, Generation: name, Host: host, Args: append([]string(nil), g.Args...), Identity: identity, ActivatedAt: clock.Now.UTC()}
	return payload, receipt, nil
}

func completeActivation(j *HandoffJournal, name string, receipt ActivationReceipt) {
	for other, record := range j.Generations {
		if other != name && record.State == "activated" {
			record.State = "superseded"
			j.Generations[other] = record
		}
	}
	j.Generations[name] = HandoffRecord{State: "activated", Receipt: &receipt}
}

// Activate authenticates/rechecks a whole generation and durably records the
// protected handoff. It never starts a process or registers a service. Expiry
// gates a first activation; an already activated generation may restart offline.
func Activate(ctx context.Context, root, generation string, clock Clock) (ServicePayload, ActivationReceipt, error) {
	var payload ServicePayload
	var receipt ActivationReceipt
	err := withPrivateTransaction(ctx, filepath.Join(root, "payload"), ".handoff.lock", func() error {
		j, err := loadHandoff(root)
		if err != nil {
			return err
		}
		if err = reconcileHandoff(ctx, root, &j, clock); err != nil {
			return err
		}
		r, ok := j.Generations[generation]
		if !ok || (r.State != "staged" && r.State != "activated") {
			return ReasonHandoffTransition
		}
		p, a, err := checkActivation(ctx, root, generation, clock, r.State == "activated")
		if err != nil {
			return err
		}
		if r.State == "staged" {
			if err = handoffTransition(&j, generation, "activating"); err != nil {
				return err
			}
			if err = saveHandoff(root, &j); err != nil {
				return err
			}
		}
		// Identity is checked once more after the durable intent write, immediately
		// before recording acceptance. The caller must also perform launch-check.
		if err = VerifyPayloadIdentity(p, a); err != nil {
			return err
		}
		completeActivation(&j, generation, a)
		if err = saveHandoff(root, &j); err != nil {
			return err
		}
		payload, receipt = p, a
		return nil
	})
	if err != nil {
		return ServicePayload{}, ActivationReceipt{}, err
	}
	return payload, receipt, nil
}

func readHostIdentity(name string) (HostIdentity, error) {
	var identity HostIdentity
	if privatePathComponents(name) != nil {
		return identity, ReasonPayloadIdentity
	}
	f, err := openHostExecutable(name)
	if err != nil {
		return identity, ReasonPayloadIdentity
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !privateOwned(info) || info.Size() < 0 || info.Size() > MaxArchiveBytes {
		return identity, ReasonPayloadIdentity
	}
	identity, err = hostFileIdentity(f, info)
	if err != nil {
		return HostIdentity{}, ReasonPayloadIdentity
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, info.Size()+1))
	if err != nil || n != info.Size() {
		return HostIdentity{}, ReasonPayloadIdentity
	}
	after, err := os.Lstat(name)
	if err != nil || !inventoryInfoSafe(after) || !os.SameFile(info, after) || after.Size() != info.Size() {
		return HostIdentity{}, ReasonPayloadIdentity
	}
	identity.Size = info.Size()
	identity.Digest = hex.EncodeToString(h.Sum(nil))
	return identity, nil
}

// VerifyPayloadIdentity reopens with no-follow semantics and refuses replacement,
// symlink swaps, content changes or invocation changes. B3 wires this only into
// the explicit harness launch-check, not the LC-S7A supervisor. Same-user writes
// after this check (including in-place writes to an open executable) remain the
// documented same-user boundary; this is not an atomic check-and-exec primitive.
func VerifyPayloadIdentity(payload ServicePayload, receipt ActivationReceipt) error {
	if receipt.Version != 1 || !validGenerationName(payload.Generation) || payload.Generation != receipt.Generation || payload.Host != receipt.Host || !slices.Equal(payload.Args, receipt.Args) {
		return ReasonPayloadIdentity
	}
	identity, err := readHostIdentity(payload.Host)
	if err != nil || identity != receipt.Identity {
		return ReasonPayloadIdentity
	}
	return nil
}
