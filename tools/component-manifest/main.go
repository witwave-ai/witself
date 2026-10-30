// Command component-manifest is offline fixture tooling, never a release product.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/localcell/trust"
)

var generateKey = func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 3072) }

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	for _, a := range args {
		if a == "--production" || strings.HasPrefix(a, "--production=") || a == "-production" || strings.HasPrefix(a, "-production=") {
			return errors.New("production key generation is not authorized before S7-F2")
		}
	}
	if len(args) == 0 {
		return errors.New("subcommand_required")
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:])
	case "build":
		return build(args[1:])
	case "sign":
		return sign(args[1:])
	case "verify":
		return verify(args[1:], out)
	default:
		return errors.New("unknown_subcommand")
	}
}

func flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}

func keygen(args []string) error {
	f := flags("keygen")
	dir := f.String("out", "", "private fixture directory")
	label := f.String("label", "", "TEST-ONLY-name")
	if f.Parse(args) != nil || f.NArg() != 0 || *dir == "" || !strings.HasPrefix(*label, "TEST-ONLY-") || len(*label) <= len("TEST-ONLY-") || filepath.Base(*label) != *label || strings.ContainsAny(*label, `\:`) {
		return errors.New("invalid_arguments")
	}
	if err := os.Mkdir(*dir, 0700); err != nil {
		return errors.New("output_exists_or_unavailable")
	}
	key, err := generateKey()
	if err != nil {
		return errors.New("key_generation_failed")
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return errors.New("invalid_key")
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return errors.New("invalid_key")
	}
	if err = os.WriteFile(filepath.Join(*dir, *label+".private.pem"), pem.EncodeToMemory(&pem.Block{Type: "TEST-ONLY PRIVATE KEY", Bytes: der}), 0600); err != nil {
		return errors.New("output_unavailable")
	}
	if err = os.WriteFile(filepath.Join(*dir, *label+".public.pem"), pem.EncodeToMemory(&pem.Block{Type: "TEST-ONLY PUBLIC KEY", Bytes: pub}), 0600); err != nil {
		return errors.New("output_unavailable")
	}
	return nil
}

func readBounded(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("input_unavailable")
	}
	defer func() { _ = f.Close() }() // Read-only close cannot affect verification.
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(b)) > maxBytes {
		return nil, errors.New("input_bound")
	}
	return b, nil
}

func privateKey(path string) (*rsa.PrivateKey, error) {
	b, err := readBounded(path, 16<<10)
	if err != nil {
		return nil, err
	}
	p, rest := pem.Decode(b)
	if p == nil || p.Type != "TEST-ONLY PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("test_only_key_required")
	}
	k, err := x509.ParsePKCS8PrivateKey(p.Bytes)
	if err != nil {
		return nil, errors.New("invalid_key")
	}
	r, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("invalid_key")
	}
	if _, err = trust.PublicKeyFromRSA(&r.PublicKey); err != nil {
		return nil, errors.New("invalid_key")
	}
	return r, nil
}

func publicKey(path string) (trust.PublicKey, error) {
	b, err := readBounded(path, 16<<10)
	if err != nil {
		return trust.PublicKey{}, err
	}
	p, rest := pem.Decode(b)
	if p == nil || (p.Type != "TEST-ONLY PUBLIC KEY" && p.Type != "PUBLIC KEY") || len(strings.TrimSpace(string(rest))) != 0 {
		return trust.PublicKey{}, errors.New("invalid_key")
	}
	k, err := x509.ParsePKIXPublicKey(p.Bytes)
	if err != nil {
		return trust.PublicKey{}, errors.New("invalid_key")
	}
	r, ok := k.(*rsa.PublicKey)
	if !ok {
		return trust.PublicKey{}, errors.New("invalid_key")
	}
	return trust.PublicKeyFromRSA(r)
}

type repeated []string

func (r *repeated) String() string     { return "" }
func (r *repeated) Set(s string) error { *r = append(*r, s); return nil }

func sign(args []string) error {
	f := flags("sign")
	var keys repeated
	f.Var(&keys, "key", "TEST-ONLY private PEM; repeat for rotation")
	kind := f.String("kind", "", "document kind")
	in := f.String("in", "", "document")
	out := f.String("out", "", "envelope")
	if f.Parse(args) != nil || f.NArg() != 0 || len(keys) == 0 || *in == "" || *out == "" {
		return errors.New("invalid_arguments")
	}
	b, err := readBounded(*in, trust.MaxComponentSetBytes)
	if err != nil {
		return err
	}
	var parsed []*rsa.PrivateKey
	for _, p := range keys {
		k, e := privateKey(p)
		if e != nil {
			return e
		}
		parsed = append(parsed, k)
	}
	envelope, err := trust.SignDocument(b, *kind, parsed...)
	if err != nil {
		return err
	}
	if err = os.WriteFile(*out, envelope, 0600); err != nil {
		return errors.New("output_unavailable")
	}
	return nil
}

func build(args []string) error {
	f := flags("build")
	dist := f.String("dist", "", "archive and component-metadata.json directory")
	tag := f.String("tag", "", "vX.Y.Z")
	seq := f.Uint64("sequence", 0, "release sequence")
	channel := f.String("channel", "", "stable")
	out := f.String("out", "", "manifest")
	var platforms repeated
	f.Var(&platforms, "platform", "os/arch=platform-template.json")
	if f.Parse(args) != nil || f.NArg() != 0 || *dist == "" || *out == "" || len(platforms) == 0 {
		return errors.New("invalid_arguments")
	}
	b, err := readBounded(filepath.Join(*dist, "component-metadata.json"), trust.MaxComponentSetBytes)
	if err != nil {
		return err
	}
	var m trust.ComponentSet
	if json.Unmarshal(b, &m) != nil {
		return errors.New("invalid_metadata")
	}
	m.ReleaseTag, m.ReleaseSequence, m.Channel = *tag, *seq, *channel
	m.Platforms = nil
	for _, spec := range platforms {
		target, file, ok := strings.Cut(spec, "=")
		if !ok {
			return errors.New("invalid_platform")
		}
		osName, arch, ok := strings.Cut(target, "/")
		if !ok {
			return errors.New("invalid_platform")
		}
		b, err = readBounded(file, trust.MaxComponentSetBytes)
		if err != nil {
			return err
		}
		var p trust.Platform
		if json.Unmarshal(b, &p) != nil || p.OS != osName || p.Arch != arch {
			return errors.New("invalid_platform")
		}
		for i := range p.Components {
			c := &p.Components[i]
			if filepath.Base(c.Archive.Name) != c.Archive.Name || strings.ContainsAny(c.Archive.Name, `\:`) {
				return errors.New("invalid_archive_name")
			}
			c.Archive, c.Inventory, err = trust.InspectArchive(context.Background(), filepath.Join(*dist, c.Archive.Name), c.Archive.Format)
			if err != nil {
				return err
			}
		}
		m.Platforms = append(m.Platforms, p)
	}
	b, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return errors.New("invalid_metadata")
	}
	if _, err = trust.ParseComponentSet(b); err != nil {
		return err
	}
	if err = os.WriteFile(*out, b, 0600); err != nil {
		return errors.New("output_unavailable")
	}
	return nil
}

func verify(args []string, out io.Writer) error {
	f := flags("verify")
	var roots, rotations repeated
	f.Var(&roots, "root", "explicit root public PEM")
	f.Var(&rotations, "rotation", "rotation document, in order")
	stateDir := f.String("state", "", "private state directory")
	delegation := f.String("delegation", "", "delegation document")
	manifest := f.String("manifest", "", "component-set document")
	now := f.String("now", "", "trusted UTC time")
	untrusted := f.Bool("untrusted-clock", false, "unknown clock")
	if f.Parse(args) != nil || f.NArg() != 0 || *manifest == "" || (*untrusted && *now != "") {
		return errors.New("invalid_arguments")
	}
	clock := trust.Clock{Now: time.Now().UTC(), Trusted: !*untrusted}
	if *now != "" {
		t, err := time.Parse("2006-01-02T15:04:05Z", *now)
		if err != nil {
			return errors.New("invalid_clock")
		}
		clock.Now = t
	}
	opts := trust.Options{Clock: clock}
	for _, path := range roots {
		k, err := publicKey(path)
		if err != nil {
			return err
		}
		opts.Roots = append(opts.Roots, k)
	}
	if *stateDir != "" {
		s, err := trust.NewFileState(*stateDir)
		if err != nil {
			return err
		}
		opts.State = s
	}
	v := trust.NewVerifier(opts)
	process := func(path string, fn func([]byte, []byte) trust.Outcome) (trust.Outcome, error) {
		b, err := readBounded(path, trust.MaxComponentSetBytes)
		if err != nil {
			return trust.Outcome{}, err
		}
		s, err := readBounded(path+".sig.json", trust.MaxEnvelopeBytes)
		if err != nil {
			return trust.Outcome{}, err
		}
		return fn(b, s), nil
	}
	emit := func(o trust.Outcome) error {
		if json.NewEncoder(out).Encode(o) != nil {
			return errors.New("output_unavailable")
		}
		if !o.Accepted {
			return errors.New(string(o.Reason))
		}
		return nil
	}
	for _, path := range rotations {
		o, err := process(path, v.VerifyRotation)
		if err != nil {
			return err
		}
		if !o.Accepted {
			return emit(o)
		}
	}
	if *delegation != "" {
		o, err := process(*delegation, v.VerifyDelegation)
		if err != nil {
			return err
		}
		if !o.Accepted {
			return emit(o)
		}
	}
	o, err := process(*manifest, v.AcceptForAcquisition)
	if err != nil {
		return err
	}
	return emit(o)
}
