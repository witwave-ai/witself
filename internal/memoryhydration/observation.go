package memoryhydration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// Outcome is a closed, value-free description of one hydration hook event.
type Outcome string

// Closed outcomes for local hydration observations.
const (
	OutcomeInjected         Outcome = "injected"
	OutcomeNoContext        Outcome = "no_context"
	OutcomeTimeout          Outcome = "timeout"
	OutcomeSelfError        Outcome = "self_error"
	OutcomeBindingMismatch  Outcome = "binding_mismatch"
	OutcomeRecallDegraded   Outcome = "recall_degraded"
	OutcomeOutputRejected   Outcome = "output_rejected"
	OutcomeConfigError      Outcome = "config_error"
	maximumObservationBytes         = 512 * 1024
)

// Observation contains only timing, sizes, flags, and a closed outcome. It must
// never acquire prompt, query, context, identity, credentials, or error text.
type Observation struct {
	Timestamp    time.Time `json:"timestamp"`
	Attempted    bool      `json:"attempted"`
	Injected     bool      `json:"injected"`
	ElapsedMS    float64   `json:"elapsed_ms"`
	ContextBytes int       `json:"context_bytes"`
	Elided       bool      `json:"elided"`
	Outcome      Outcome   `json:"outcome"`
}

// NewObservation copies the allowlisted measurements, never Result's content.
func NewObservation(result Result) Observation {
	outcome := result.Outcome
	if !validOutcome(outcome) {
		outcome = OutcomeConfigError
	}
	return Observation{Timestamp: time.Now().UTC(), Attempted: result.Attempted, Injected: result.Injected, ElapsedMS: max(0, float64(result.Elapsed)/float64(time.Millisecond)), ContextBytes: max(0, result.ContextBytes), Elided: result.Elided, Outcome: outcome}
}

// ObservationSummary aggregates bounded local evidence. Attempts counts hook
// events, including configuration failures before a network read. Degraded
// recall can count as both injected and failed because its notice was delivered.
type ObservationSummary struct {
	Attempts           int     `json:"attempts"`
	Injected           int     `json:"injected"`
	Failures           int     `json:"failures"`
	ElidedCount        int     `json:"elided_count"`
	HookOutputRejected int     `json:"hook_output_rejected"`
	P95LatencyMS       float64 `json:"p95_latency_ms"`
	MaxLatencyMS       float64 `json:"max_latency_ms"`
}

// SummarizeObservations returns value-free counts and nearest-rank p95 latency.
func SummarizeObservations(observations []Observation) ObservationSummary {
	var summary ObservationSummary
	latencies := make([]float64, 0, len(observations))
	for _, observation := range observations {
		if !validObservation(observation) {
			continue
		}
		summary.Attempts++
		if observation.Injected {
			summary.Injected++
		}
		if observation.Outcome != OutcomeInjected && observation.Outcome != OutcomeNoContext {
			summary.Failures++
		}
		if observation.Elided {
			summary.ElidedCount++
		}
		if observation.Outcome == OutcomeOutputRejected {
			summary.HookOutputRejected++
		}
		summary.MaxLatencyMS = max(summary.MaxLatencyMS, observation.ElapsedMS)
		latencies = append(latencies, observation.ElapsedMS)
	}
	if len(latencies) > 0 {
		slices.Sort(latencies)
		summary.P95LatencyMS = latencies[int(math.Ceil(float64(len(latencies))*0.95))-1]
	}
	return summary
}

// AppendObservation writes one private JSONL record. The ledger retains recent
// complete lines within 512 KiB, independently of the removable capture outbox.
// File locking serializes concurrent hook processes; contention is bounded so a
// ledger problem cannot indefinitely delay fail-open context delivery.
func AppendObservation(runtime string, observation Observation) error {
	if !validObservation(observation) {
		return errors.New("invalid hydration observation")
	}
	path, err := observationPath(runtime)
	if err != nil {
		return err
	}
	line, err := json.Marshal(observation)
	if err != nil {
		return errors.New("encode hydration observation")
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := openObservationFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := lockObservationFile(file); err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(line)) > maximumObservationBytes {
		tail, err := readObservationTail(file, maximumObservationBytes/2)
		if err != nil {
			return err
		}
		if err := file.Truncate(0); err != nil {
			return err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := file.Write(tail); err != nil {
			return err
		}
	} else if info.Size() > 0 {
		var last [1]byte
		if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			// Repair a partially written final record before appending so the
			// next hook observation remains a complete independent JSON line.
			tail, err := readObservationTail(file, maximumObservationBytes)
			if err != nil {
				return err
			}
			if err := file.Truncate(int64(len(tail))); err != nil {
				return err
			}
		}
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if _, err := file.Write(line); err != nil {
		return err
	}
	return nil
}

// ReadObservations reads valid allowlisted records in the inclusive time window.
// A zero bound is open-ended. Missing ledgers are normal for older integrations
// and guided fallback runtimes; corrupt or foreign records are ignored.
func ReadObservations(runtime string, from, until time.Time) ([]Observation, error) {
	path, err := observationPath(runtime)
	if err != nil {
		return nil, err
	}
	file, err := openObservationFile(path, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if err := lockObservationFile(file); err != nil {
		return nil, err
	}
	raw, err := readObservationTail(file, maximumObservationBytes)
	if err != nil {
		return nil, err
	}
	var observations []Observation
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var observation Observation
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&observation) != nil || !validObservation(observation) {
			continue
		}
		if decoder.Decode(new(any)) != io.EOF {
			continue
		}
		if (!from.IsZero() && observation.Timestamp.Before(from)) || (!until.IsZero() && observation.Timestamp.After(until)) {
			continue
		}
		observations = append(observations, observation)
	}
	return observations, nil
}

func validOutcome(outcome Outcome) bool {
	switch outcome {
	case OutcomeInjected, OutcomeNoContext, OutcomeTimeout, OutcomeSelfError, OutcomeBindingMismatch, OutcomeRecallDegraded, OutcomeOutputRejected, OutcomeConfigError:
		return true
	default:
		return false
	}
}

func validObservation(observation Observation) bool {
	return !observation.Timestamp.IsZero() && validOutcome(observation.Outcome) && observation.ElapsedMS >= 0 && !math.IsNaN(observation.ElapsedMS) && !math.IsInf(observation.ElapsedMS, 0) && observation.ContextBytes >= 0 && observation.ContextBytes <= MaximumContextBytes
}

func observationPath(runtime string) (string, error) {
	if !slices.Contains(transcriptcapture.SupportedRuntimes(), runtime) {
		return "", errors.New("unsupported hydration ledger runtime")
	}
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "capture", "hydration", runtime+".jsonl"), nil
}

func openObservationFile(path string, flags int) (*os.File, error) {
	before, err := os.Lstat(path)
	if err == nil && !before.Mode().IsRegular() {
		return nil, errors.New("hydration ledger must be a regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	linked, linkErr := os.Lstat(path)
	if err != nil || linkErr != nil || !opened.Mode().IsRegular() || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		_ = file.Close()
		return nil, errors.New("hydration ledger file changed while opening")
	}
	return file, nil
}

func readObservationTail(file *os.File, maximumBytes int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := max(0, info.Size()-maximumBytes)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximumBytes))
	if err != nil {
		return nil, err
	}
	if start > 0 {
		if newline := bytes.IndexByte(raw, '\n'); newline >= 0 {
			raw = raw[newline+1:]
		} else {
			raw = nil
		}
	}
	// Never carry an incomplete record into a future append.
	if end := bytes.LastIndexByte(raw, '\n'); end >= 0 {
		raw = raw[:end+1]
	} else {
		raw = nil
	}
	return raw, nil
}
