package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/localcell/trust"
)

// The operator supplies test roots explicitly. There is no ambient root or
// environment-variable activation, no candidate execution, and no networking.
func trustCommandClock() trust.Clock { return trust.Clock{Now: time.Now().UTC(), Trusted: true} }

type trustCommandFlags struct {
	root, bundle, rootPEM, os, arch, generation string
	channel, identity, dataRoot, pgdata         string
	owned, writes                               repeatedPaths
	revision                                    uint64
	launchCheck                                 bool
}

type repeatedPaths []string

func (p *repeatedPaths) String() string { return "" }
func (p *repeatedPaths) Set(value string) error {
	if len(*p) >= 64 || len(value) > 1024 {
		return errors.New("invalid_arguments")
	}
	*p = append(*p, value)
	return nil
}

func parseTrustCommand(args []string) (trustCommandFlags, error) {
	var values trustCommandFlags
	f := flag.NewFlagSet("localcellharness trust", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&values.root, "root", "", "explicit absolute private test root")
	f.StringVar(&values.bundle, "component-set-dir", "", "explicit local signed component-set bundle")
	f.StringVar(&values.rootPEM, "trust-root-pem", "", "explicit TEST-ONLY public trust root")
	f.StringVar(&values.os, "os", runtime.GOOS, "declared target OS; no executable is launched")
	f.StringVar(&values.arch, "arch", runtime.GOARCH, "declared target architecture")
	f.StringVar(&values.generation, "generation", "", "explicit payload generation")
	f.StringVar(&values.channel, "channel", "", "declared owning channel")
	f.StringVar(&values.identity, "installing-identity", "", "caller-declared installing uid or SID")
	f.StringVar(&values.dataRoot, "data-root", "", "explicit data root for ownership overlap checks")
	f.StringVar(&values.pgdata, "pgdata", "", "explicit PGDATA for ownership overlap checks")
	f.Var(&values.owned, "owned-path", "declared absolute channel-owned path; repeat up to 64")
	f.Var(&values.writes, "write-path", "candidate absolute write path; repeat up to 64")
	f.Uint64Var(&values.revision, "revision", 0, "next explicit channel receipt revision")
	f.BoolVar(&values.launchCheck, "launch-check", false, "verify payload identity after activation; does not launch")
	if f.Parse(args) != nil || f.NArg() != 0 {
		return values, errors.New("invalid_arguments")
	}
	if values.rootPEM == "" {
		return values, trust.ReasonNoTrustRoot
	}
	if !filepath.IsAbs(values.root) || values.root != filepath.Clean(values.root) || values.bundle == "" {
		return values, errors.New("invalid_arguments")
	}
	return values, nil
}

type harnessBundle struct {
	rootKey    trust.PublicKey
	verifier   *trust.Verifier
	state      *trust.FileState
	set        trust.VerifiedSet
	manifest   trust.ComponentSet
	components []trust.Component
	archives   map[string]string
}

// readTrustInput bounds memory use and rejects nonregular/symlink inputs before
// parsing. Error text deliberately omits paths, document bytes and key values.
func readTrustInput(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !harnessTrustInputSafe(info) || info.Size() > limit {
		return nil, errors.New("input_unavailable")
	}
	f, err := openHarnessTrustInput(path)
	if err != nil {
		return nil, errors.New("input_unavailable")
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	after, afterErr := os.Lstat(path)
	if err != nil || afterErr != nil || !opened.Mode().IsRegular() || !harnessTrustInputSafe(opened) || !harnessTrustInputSafe(after) || !os.SameFile(info, opened) || !os.SameFile(after, opened) {
		return nil, errors.New("input_unavailable")
	}
	value, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(value)) > limit {
		return nil, errors.New("input_bound")
	}
	return value, nil
}

func readHarnessPublicRoot(path string) (trust.PublicKey, error) {
	raw, err := readTrustInput(path, trust.MaxAuthorityBytes)
	if err != nil {
		return trust.PublicKey{}, err
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "TEST-ONLY PUBLIC KEY" || len(rest) != 0 || len(block.Headers) != 0 {
		return trust.PublicKey{}, trust.ReasonInvalidKey
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return trust.PublicKey{}, trust.ReasonInvalidKey
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return trust.PublicKey{}, trust.ReasonInvalidKey
	}
	return trust.PublicKeyFromRSA(key)
}

func readHarnessPair(dir, name string, limit int64) ([]byte, []byte, error) {
	doc, err := readTrustInput(filepath.Join(dir, name), limit)
	if err != nil {
		return nil, nil, err
	}
	envelope, err := readTrustInput(filepath.Join(dir, name+".sig.json"), trust.MaxEnvelopeBytes)
	return doc, envelope, err
}

func loadHarnessBundle(values trustCommandFlags, clock trust.Clock, acquire bool) (harnessBundle, error) {
	var bundle harnessBundle
	key, err := readHarnessPublicRoot(values.rootPEM)
	if err != nil {
		return bundle, err
	}
	bundle.rootKey = key
	doc, envelope, err := readHarnessPair(values.bundle, "component-set.json", trust.MaxComponentSetBytes)
	if err != nil {
		return bundle, err
	}
	manifest, err := trust.ParseComponentSet(doc)
	if err != nil {
		return bundle, err
	}
	for _, platform := range manifest.Platforms {
		if platform.OS == values.os && platform.Arch == values.arch {
			bundle.components = platform.Components
		}
	}
	if len(bundle.components) == 0 {
		return bundle, trust.ReasonWrongPlatform
	}
	if err := harnessPrivateDirectory(values.root); err != nil {
		return bundle, trust.ReasonStateIO
	}
	state, err := trust.NewFileState(filepath.Join(values.root, "trust"))
	if err != nil {
		return bundle, err
	}
	opts := trust.Options{Roots: []trust.PublicKey{key}, State: state, Clock: clock, OS: values.os, Arch: values.arch}
	v := trust.NewVerifier(opts)
	if !acquire {
		outcome := v.AcceptForOfflineStart(doc, envelope)
		if outcome.Accepted {
			bundle.verifier, bundle.state, bundle.manifest = v, state, manifest
			bundle.set = trust.VerifiedSet{Document: doc, Envelope: envelope}
			return bundle, nil
		}
		if outcome.Reason != trust.ReasonNotInstalled {
			return bundle, outcome.Reason
		}
		// Inspection and ownership declarations may authenticate a fresh bundle,
		// but do not commit installation eligibility or alter acquisition state.
		snapshot, loadErr := state.Load()
		if loadErr != nil {
			return bundle, loadErr
		}
		memory := trust.NewMemoryState()
		if err := memory.Update(func(s *trust.StateSnapshot) error { *s = snapshot; return nil }); err != nil {
			return bundle, err
		}
		opts.State = memory
		v = trust.NewVerifier(opts)
	}
	if err := harnessBundleAuthorities(v, opts.State, values.bundle, key, clock); err != nil {
		return bundle, err
	}
	outcome := v.VerifyComponentSet(doc, envelope)
	if acquire {
		outcome = v.AcceptSet(doc, envelope, bundle.components)
	}
	if !outcome.Accepted {
		return bundle, outcome.Reason
	}
	bundle.verifier, bundle.state, bundle.manifest = v, state, manifest
	bundle.set = trust.VerifiedSet{Document: doc, Envelope: envelope}
	bundle.archives = make(map[string]string, len(bundle.components))
	for _, component := range bundle.components {
		bundle.archives[component.Role] = filepath.Join(values.bundle, component.Archive.Name)
	}
	return bundle, nil
}

func harnessBundleAuthorities(verifier *trust.Verifier, state trust.TrustState, directory string, root trust.PublicKey, clock trust.Clock) error {
	if err := validateHarnessRotationNames(directory); err != nil {
		return err
	}
	snapshot, err := state.Load()
	if err != nil {
		return err
	}
	rotationState := trust.NewMemoryState()
	rotationVerifier := trust.NewVerifier(trust.Options{Roots: []trust.PublicKey{root}, State: rotationState, Clock: clock})
	// Numbered rotations are bounded and consecutive. Even an unexpected later
	// pair refuses rather than silently being ignored after a gap.
	for index, missing := 1, false; index <= 33; index++ {
		name := fmt.Sprintf("root-rotation-%d.json", index)
		_, docErr := os.Lstat(filepath.Join(directory, name))
		_, envelopeErr := os.Lstat(filepath.Join(directory, name+".sig.json"))
		if errors.Is(docErr, os.ErrNotExist) && errors.Is(envelopeErr, os.ErrNotExist) {
			missing = true
			continue
		}
		if missing || index > 32 || docErr != nil || envelopeErr != nil {
			return trust.ReasonInvalidDocument
		}
		doc, envelope, err := readHarnessPair(directory, name, trust.MaxAuthorityBytes)
		if err != nil {
			return err
		}
		outcome := rotationVerifier.VerifyRotation(doc, envelope)
		if !outcome.Accepted {
			return outcome.Reason
		}
		if outcome.RootSequence <= snapshot.RootSequence {
			rotationSnapshot, err := rotationState.Load()
			if err != nil || outcome.RootSequence > uint64(len(snapshot.RootHistory)) || snapshot.InitialRootID != root.KeyID || snapshot.RootHistory[outcome.RootSequence-1] != rotationSnapshot.Root.KeyID {
				return trust.ReasonStateInvalid
			}
			continue
		}
		if outcome := verifier.VerifyRotation(doc, envelope); !outcome.Accepted {
			return outcome.Reason
		}
	}
	doc, envelope, err := readHarnessPair(directory, "delegation.json", trust.MaxAuthorityBytes)
	if err != nil {
		return err
	}
	if outcome := verifier.VerifyDelegation(doc, envelope); !outcome.Accepted {
		return outcome.Reason
	}
	return nil
}

func validateHarnessRotationNames(directory string) error {
	dir, err := os.Open(directory)
	if err != nil {
		return errors.New("input_unavailable")
	}
	defer func() { _ = dir.Close() }()
	for count := 0; ; {
		names, err := dir.Readdirnames(128)
		count += len(names)
		if count > 4096 {
			return errors.New("input_bound")
		}
		for _, name := range names {
			if !strings.HasPrefix(name, "root-rotation-") {
				continue
			}
			stem := strings.TrimSuffix(strings.TrimSuffix(name, ".sig.json"), ".json")
			index, parseErr := strconv.Atoi(strings.TrimPrefix(stem, "root-rotation-"))
			if parseErr != nil || index < 1 || index > 32 || (name != fmt.Sprintf("root-rotation-%d.json", index) && name != fmt.Sprintf("root-rotation-%d.json.sig.json", index)) {
				return trust.ReasonInvalidDocument
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.New("input_unavailable")
		}
	}
}

func runTrustCommand(ctx context.Context, args []string, out io.Writer, clock trust.Clock) error {
	if len(args) < 2 {
		return errors.New("invalid_arguments")
	}
	group, action := args[0], args[1]
	if (group != "payload" || (action != "stage" && action != "activate" && action != "retire" && action != "inspect")) &&
		(group != "channel" || (action != "receipt" && action != "inspect" && action != "preflight")) {
		return errors.New("invalid_arguments")
	}
	values, err := parseTrustCommand(args[2:])
	if err != nil {
		return err
	}
	if values.launchCheck && (group != "payload" || action != "activate") {
		return errors.New("invalid_arguments")
	}
	if group == "payload" && (values.channel != "" || values.identity != "" || values.revision != 0 || len(values.owned) != 0 || len(values.writes) != 0 || values.dataRoot != "" || values.pgdata != "") {
		return errors.New("invalid_arguments")
	}
	if group == "payload" && (action == "stage" || action == "inspect") && values.generation != "" {
		return errors.New("invalid_arguments")
	}
	if group == "channel" && (values.generation != "" || (action != "receipt" && (values.channel != "" || values.identity != "" || values.revision != 0 || len(values.owned) != 0))) {
		return errors.New("invalid_arguments")
	}
	if group == "channel" && ((action != "preflight" && len(values.writes) != 0) || (action == "preflight" && len(values.writes) == 0)) {
		return errors.New("invalid_arguments")
	}
	if group == "channel" && action == "receipt" && (values.channel == "" || values.identity == "" || values.revision == 0) {
		return errors.New("invalid_arguments")
	}
	if group == "payload" && (action == "activate" || action == "retire") && values.generation == "" {
		return errors.New("invalid_arguments")
	}
	if err := ctx.Err(); err != nil {
		return errors.New("operation_canceled")
	}
	bundle, err := loadHarnessBundle(values, clock, group == "payload" && action == "stage")
	if err != nil {
		return err
	}
	if group == "channel" {
		return runChannelTrustCommand(ctx, action, values, bundle, out, clock)
	}
	return runPayloadTrustCommand(ctx, action, values, bundle, out, clock)
}

func runPayloadTrustCommand(ctx context.Context, action string, values trustCommandFlags, bundle harnessBundle, out io.Writer, clock trust.Clock) error {
	switch action {
	case "stage":
		if _, err := trust.StageGeneration(ctx, bundle.verifier, bundle.state, bundle.set, bundle.components, bundle.archives, values.root); err != nil {
			return err
		}
	case "activate":
		if err := checkHarnessGeneration(values, bundle, clock); err != nil {
			return err
		}
		payload, receipt, err := trust.Activate(ctx, values.root, values.generation, clock)
		if err != nil {
			return err
		}
		if values.launchCheck {
			if err := trust.VerifyPayloadIdentity(payload, receipt); err != nil {
				return err
			}
		}
	case "retire":
		if err := checkHarnessGeneration(values, bundle, clock); err != nil {
			return err
		}
		if err := trust.Retire(ctx, values.root, values.generation); err != nil {
			return err
		}
	case "inspect":
		journal, err := trust.InspectHandoff(ctx, values.root)
		if err != nil {
			return err
		}
		counts := map[string]int{}
		for _, record := range journal.Generations {
			counts[record.State]++
		}
		return json.NewEncoder(out).Encode(struct {
			States map[string]int `json:"states"`
		}{counts})
	}
	return json.NewEncoder(out).Encode(struct {
		Operation string `json:"operation"`
		State     string `json:"state"`
	}{"payload " + action, "done"})
}

func checkHarnessGeneration(values trustCommandFlags, bundle harnessBundle, clock trust.Clock) error {
	generation, err := trust.LoadGeneration(values.root, values.generation)
	if err != nil {
		return err
	}
	if !bytes.Equal(generation.Document, bundle.set.Document) || !bytes.Equal(generation.Envelope, bundle.set.Envelope) {
		return trust.ReasonMixedSet
	}
	state := trust.NewMemoryState()
	if err := state.Update(func(s *trust.StateSnapshot) error { *s = generation.Trust; return nil }); err != nil {
		return err
	}
	v := trust.NewVerifier(trust.Options{Roots: []trust.PublicKey{bundle.rootKey}, State: state, Clock: clock, OS: values.os, Arch: values.arch})
	if outcome := v.AcceptForOfflineStart(generation.Document, generation.Envelope); !outcome.Accepted {
		return outcome.Reason
	}
	return nil
}

func runChannelTrustCommand(ctx context.Context, action string, values trustCommandFlags, bundle harnessBundle, out io.Writer, clock trust.Clock) error {
	if action == "receipt" {
		digest := sha256.Sum256(bundle.set.Document)
		receipt := trust.ChannelReceipt{Channel: values.channel, OwnedPaths: values.owned, InstallingIdentity: values.identity, DeclaredAt: clock.Now, Revision: values.revision,
			ComponentSet: &trust.ComponentSetIdentity{ManifestDigest: hex.EncodeToString(digest[:]), Channel: bundle.manifest.Channel, Sequence: bundle.manifest.ReleaseSequence, Tag: bundle.manifest.ReleaseTag}}
		if err := trust.WriteChannelReceipt(ctx, values.root, receipt); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(struct {
			Operation string `json:"operation"`
			State     string `json:"state"`
		}{"channel receipt", "done"})
	}
	receipt, err := trust.ReadChannelReceipt(values.root)
	if err != nil {
		return err
	}
	dataRoot, pgdata := values.dataRoot, values.pgdata
	if dataRoot == "" {
		dataRoot = values.root
	}
	if pgdata == "" {
		pgdata = filepath.Join(values.root, "pg")
	}
	adapter := trust.NewChannelAdapter(receipt.Channel, trust.ChannelLayout{DataRoot: dataRoot, PGDATA: pgdata, PayloadRoot: filepath.Join(values.root, "payload")})
	if action == "inspect" {
		observation, err := adapter.Inspect(ctx, receipt)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(observation)
	}
	decision, err := adapter.PreflightUpdate(ctx, receipt, trust.ChannelCandidate{WritePaths: values.writes})
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(decision)
}

// Keep a single fixed output vocabulary even if an implementation later adds
// caller-valued error detail. Current trust reasons are already value-free.
func safeTrustError(err error) error {
	if err == nil {
		return nil
	}
	for _, character := range err.Error() {
		if character != '_' && (character < 'a' || character > 'z') {
			return errors.New("trust_operation_refused")
		}
	}
	return err
}
