package trust

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// Handoff refusals are fixed codes; local state and paths never enter errors.
const (
	ReasonHandoffTransition             Reason = "handoff_transition_invalid"
	ReasonGenerationReferenced          Reason = "generation_referenced"
	ReasonProcessObservationUnavailable Reason = "process_observation_unavailable"
)

// HandoffRecord records lifecycle intent only, never package-manager success.
type HandoffRecord struct {
	State   string             `json:"state"`
	Receipt *ActivationReceipt `json:"receipt,omitempty"`
}

// HandoffJournal is a bounded private journal, independently fenced from trust state.
type HandoffJournal struct {
	Version     int                      `json:"version"`
	Revision    uint64                   `json:"revision"`
	Generations map[string]HandoffRecord `json:"generations"`
}

func privatePathComponents(name string) error {
	if name == "" || !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return ReasonStateIO
	}
	for current := name; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ReasonStateIO
		}
		if err == nil && (!inventoryInfoSafe(info) || (current != name && !info.IsDir())) {
			return ReasonStateIO
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func privateReadDirectory(dir string) error {
	if runtime.GOOS == "windows" || privatePathComponents(dir) != nil {
		return ReasonStateIO
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || (info.Mode().Perm() != 0700 && info.Mode().Perm() != 0500) || !privateOwned(info) {
		return ReasonStateIO
	}
	return nil
}

func privateDirectory(dir string, create bool) error {
	if runtime.GOOS == "windows" || privatePathComponents(dir) != nil {
		return ReasonStateIO
	}
	if create {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return ReasonStateIO
		}
	}
	if privateReadDirectory(dir) != nil {
		return ReasonStateIO
	}
	info, err := os.Lstat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		return ReasonStateIO
	}
	return nil
}

// Local records carry exact signed documents as base64, so they have a separate
// bounded JSON profile from the frozen signed-document profile.
func localJSONProfile(b []byte) error {
	if !utf8.Valid(b) || !validSurrogates(b) {
		return ReasonStateInvalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return ReasonStateInvalid
		}
		token, err := d.Token()
		if err != nil {
			return ReasonStateInvalid
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return ReasonStateInvalid
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return ReasonStateInvalid
				}
				seen[k] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ReasonStateInvalid
		}
		_, err = d.Token()
		if err != nil {
			return ReasonStateInvalid
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ReasonStateInvalid
	}
	return nil
}

func readPrivateJSON(dir, name string, out any) error {
	if filepath.Base(name) != name || privateReadDirectory(dir) != nil {
		return ReasonStateIO
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !privateOwned(info) || info.Size() > maxStateBytes {
		return ReasonStateIO
	}
	f, err := openArchiveFile(path)
	if err != nil {
		return ReasonStateIO
	}
	defer func() { _ = f.Close() }()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return ReasonStateIO
	}
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil || len(b) > maxStateBytes {
		return ReasonStateIO
	}
	if err := localJSONProfile(b); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ReasonStateInvalid
	}
	return nil
}

func withPrivateTransaction(ctx context.Context, dir, lockName string, fn func() error) error {
	if ctx.Err() != nil || filepath.Base(lockName) != lockName || privateDirectory(dir, false) != nil {
		return ReasonStateIO
	}
	lockPath := filepath.Join(dir, lockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ReasonStateIO
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(lockPath)
		return ReasonStateIO
	}
	defer func() { _ = os.Remove(lockPath) }()
	return fn()
}

func writePrivateJSON(dir, name string, value any) error {
	if filepath.Base(name) != name || privateDirectory(dir, false) != nil {
		return ReasonStateIO
	}
	b, err := json.Marshal(value)
	if err != nil || len(b) > maxStateBytes {
		return ReasonStateInvalid
	}
	f, err := os.CreateTemp(dir, ".handoff-tmp-")
	if err != nil {
		return ReasonStateIO
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ReasonStateIO
	}
	if privateDirectory(dir, false) != nil || os.Rename(f.Name(), filepath.Join(dir, name)) != nil {
		return ReasonStateIO
	}
	d, err := os.Open(dir)
	if err != nil {
		return ReasonStateIO
	}
	err = d.Sync()
	closeErr = d.Close()
	if err != nil || closeErr != nil {
		return ReasonStateIO
	}
	return nil
}

func loadHandoff(root string) (HandoffJournal, error) {
	j := HandoffJournal{Version: 1, Generations: map[string]HandoffRecord{}}
	err := readPrivateJSON(filepath.Join(root, "payload"), "handoff.json", &j)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if j.Version != 1 || j.Generations == nil || len(j.Generations) > 1024 {
		return j, ReasonStateInvalid
	}
	active, activating := 0, 0
	for name, r := range j.Generations {
		if !validGenerationName(name) {
			return j, ReasonStateInvalid
		}
		switch r.State {
		case "staged", "activating", "faulted", "retired":
		case "activated", "superseded":
			if r.Receipt == nil || r.Receipt.Generation != name {
				return j, ReasonStateInvalid
			}
		default:
			return j, ReasonStateInvalid
		}
		if r.State == "activated" {
			active++
		}
		if r.State == "activating" {
			activating++
		}
	}
	if active > 1 || activating > 1 {
		return j, ReasonStateInvalid
	}
	return j, nil
}

func saveHandoff(root string, j *HandoffJournal) error {
	if j.Revision == ^uint64(0) {
		return ReasonStateInvalid
	}
	j.Revision++
	return writePrivateJSON(filepath.Join(root, "payload"), "handoff.json", j)
}

// Persist the caller's accepted authority separately from each generation's
// historical receipt. A first activation must consult this current high-water
// state, including revocations accepted after its generation was staged.
func persistGenerationTrust(root string, g Generation) error {
	dir := filepath.Join(root, "trust")
	if err := privateDirectory(dir, true); err != nil {
		return err
	}
	state, err := NewFileState(dir)
	if err != nil {
		return err
	}
	return state.Update(func(current *StateSnapshot) error {
		snapshot := cloneState(g.Trust)
		v := generationVerifier(g, Clock{})
		if err := v.validateState(&snapshot); err != nil {
			return err
		}
		if current.RootSequence == 0 {
			*current = snapshot
			return nil
		}
		if err := v.validateState(current); err != nil {
			return err
		}
		if snapshot.InitialRootID != current.InitialRootID {
			return ReasonStateInvalid
		}
		// A stale caller cannot rewind authority. Equal sequences cannot replace
		// the root/delegation document already committed by another operation.
		if snapshot.RootSequence < current.RootSequence || snapshot.DelegationSequence < current.DelegationSequence {
			return ReasonReplay
		}
		for i, id := range current.RootHistory {
			if i >= len(snapshot.RootHistory) || snapshot.RootHistory[i] != id {
				return ReasonStateInvalid
			}
		}
		if snapshot.DelegationSequence == current.DelegationSequence && snapshot.DelegationDigest != current.DelegationDigest {
			return ReasonSequence
		}
		for channel, prior := range current.Releases {
			next, ok := snapshot.Releases[channel]
			if !ok || next.Sequence < prior.Sequence {
				snapshot.Releases[channel] = prior
			} else if next.Sequence == prior.Sequence && (next.Digest != prior.Digest || next.Tag != prior.Tag) {
				return ReasonSequence
			}
		}
		for digest, record := range current.Installed {
			if other, ok := snapshot.Installed[digest]; ok && other != record {
				return ReasonStateInvalid
			}
			snapshot.Installed[digest] = record
		}
		if err := v.validateState(&snapshot); err != nil {
			return err
		}
		*current = snapshot
		return nil
	})
}

func stageHandoff(root, name string) error {
	j, err := loadHandoff(root)
	if err != nil {
		return err
	}
	if _, ok := j.Generations[name]; ok {
		return ReasonHandoffTransition
	}
	if len(j.Generations) >= 1024 {
		return ReasonStateInvalid
	}
	j.Generations[name] = HandoffRecord{State: "staged"}
	return saveHandoff(root, &j)
}

func handoffTransition(j *HandoffJournal, name, next string) error {
	r, ok := j.Generations[name]
	if !ok {
		return ReasonHandoffTransition
	}
	legal := next == "faulted" || r.State == "staged" && next == "activating" || r.State == "activating" && next == "activated" || r.State == "activated" && next == "superseded" || r.State == "superseded" && next == "retired"
	if !legal || r.State == next {
		return ReasonHandoffTransition
	}
	r.State = next
	j.Generations[name] = r
	return nil
}

func reconcileHandoff(ctx context.Context, root string, j *HandoffJournal, clock Clock) error {
	for name, r := range j.Generations {
		if r.State != "activating" {
			continue
		}
		_, receipt, err := checkActivation(ctx, root, name, clock, false)
		if err != nil {
			r.State = "faulted"
			r.Receipt = nil
			j.Generations[name] = r
		} else {
			completeActivation(j, name, receipt)
		}
		if err := saveHandoff(root, j); err != nil {
			return err
		}
	}
	return nil
}

// InspectHandoff reconciles interrupted activation intents before observing state.
func InspectHandoff(ctx context.Context, root string) (HandoffJournal, error) {
	var j HandoffJournal
	err := withPrivateTransaction(ctx, filepath.Join(root, "payload"), ".handoff.lock", func() error {
		var err error
		j, err = loadHandoff(root)
		if err != nil {
			return err
		}
		return reconcileHandoff(ctx, root, &j, Clock{Now: time.Now().UTC(), Trusted: true})
	})
	return j, err
}

// Retire removes only an unreferenced superseded generation. Unknown process
// visibility refuses retirement; it never claims that a failed probe found none.
func Retire(ctx context.Context, root, generation string) error {
	return retireGeneration(ctx, root, generation, generationProcessReferenced)
}

func retireGeneration(ctx context.Context, root, generation string, probe func(context.Context, string) (bool, error)) error {
	return withPrivateTransaction(ctx, filepath.Join(root, "payload"), ".handoff.lock", func() error {
		if err := checkPayloadOwnership(root); err != nil {
			return err
		}
		j, err := loadHandoff(root)
		if err != nil {
			return err
		}
		if err = reconcileHandoff(ctx, root, &j, Clock{Now: time.Now().UTC(), Trusted: true}); err != nil {
			return err
		}
		r, ok := j.Generations[generation]
		if !ok {
			return ReasonHandoffTransition
		}
		if r.State == "activated" {
			return ReasonGenerationReferenced
		}
		if r.State != "superseded" && r.State != "retired" {
			return ReasonHandoffTransition
		}
		path := filepath.Join(root, "payload", generation)
		if r.State == "retired" {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				return nil
			}
		}
		var g Generation
		if r.State == "retired" {
			// A crash after the durable retirement but before removal may have
			// changed the owned container back to 0700. Revalidate its receipt
			// and references before resuming only that generation's cleanup.
			err = readPrivateJSON(path, "generation.json", &g)
			if err == nil {
				err = validateGeneration(g)
			}
			if err == nil && g.Name != generation {
				err = ReasonGenerationInvalid
			}
		} else {
			g, err = LoadGeneration(root, generation)
		}
		if err != nil {
			return err
		}
		if err = descriptorReferences(root, generation); err != nil {
			return err
		}
		if r.Receipt == nil {
			return ReasonStateInvalid
		}
		payload := ServicePayload{Generation: generation, Host: filepath.Join(root, "payload", generation, filepath.FromSlash(g.Host)), Args: g.Args}
		if err = VerifyPayloadIdentity(payload, *r.Receipt); err != nil {
			return err
		}
		referenced, err := probe(ctx, filepath.Join(root, "payload", generation, filepath.FromSlash(g.Host)))
		if err != nil {
			return err
		}
		if referenced {
			return ReasonGenerationReferenced
		}
		if r.State != "retired" {
			if err = handoffTransition(&j, generation, "retired"); err != nil {
				return err
			}
			if err = saveHandoff(root, &j); err != nil {
				return err
			}
		}
		if err = os.Chmod(path, 0700); err != nil {
			return ReasonStateIO
		}
		if err = os.RemoveAll(path); err != nil {
			return ReasonStateIO
		}
		return nil
	})
}

func descriptorReferences(root, generation string) error {
	dir := filepath.Join(root, "service")
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	// Keep trust standard-library-only: inspect the stable descriptor JSON seam,
	// conservatively retaining even a stopped descriptor's referenced generation.
	var fields map[string]json.RawMessage
	if err := readPrivateJSON(dir, "descriptor.json", &fields); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var payload struct {
		Generation string   `json:"generation"`
		Host       string   `json:"host"`
		Args       []string `json:"args"`
	}
	if json.Unmarshal(fields["payload"], &payload) != nil || strings.TrimSpace(payload.Generation) == "" {
		return ReasonStateInvalid
	}
	if payload.Generation == generation {
		return ReasonGenerationReferenced
	}
	return nil
}
