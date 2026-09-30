// Command fixture-loader builds synthetic transcript accounts for operator rehearsals.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

type deps struct {
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	run    func(context.Context, string, []string) ([]byte, int, error)
	client *http.Client
	stdout io.Writer
	stderr io.Writer
}
type options struct {
	entriesPerTranscript     int
	floorBytes, ceilingBytes int64
}

func productionOptions() options { return options{5000, 367001600, 500000000} }
func defaultDeps() deps {
	return deps{time.Now, contextSleep, execRunner, newHTTPClient(), os.Stdout, os.Stderr}
}
func contextSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli(ctx, os.Args[1:], defaultDeps())
	stop()
	os.Exit(code)
}
func cli(ctx context.Context, args []string, d deps) int {
	return cliWithOptions(ctx, args, d, productionOptions())
}

type toolError struct {
	code int
	msg  string
}

func (e *toolError) Error() string { return e.msg }
func failure(code int, format string, args ...any) *toolError {
	return &toolError{code, fmt.Sprintf(format, args...)}
}
func kv(d deps, key string, value any) { _, _ = fmt.Fprintf(d.stdout, "%-26s%v\n", key, value) }

const usage = "usage: fixture-loader check|mark|load|measure|close [flags]"

var accountPattern = regexp.MustCompile(`^acc_[a-z2-7]{16}$`)

type config struct {
	verb, account, agentTokenFile, operatorTokenFile, expectCell, controlPlane, kubeContext, kubeconfig, kubectl string
	yes, dryRun, noMeasure                                                                                       bool
	founder, entries, rate                                                                                       int64
	maxMeasures                                                                                                  int
	limits                                                                                                       thresholds
}

func parseConfig(args []string) (config, error) {
	c := config{controlPlane: "https://self.witwave.ai", kubectl: "kubectl", rate: 1000000, maxMeasures: 2, limits: thresholds{90, 5, 70, 75}}
	if len(args) == 0 {
		return c, failure(2, usage)
	}
	c.verb = args[0]
	switch c.verb {
	case "check", "mark", "load", "measure", "close":
	default:
		return c, failure(2, usage)
	}
	f := flag.NewFlagSet(c.verb, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	if c.verb != "check" {
		f.StringVar(&c.account, "account", "", "")
		f.StringVar(&c.operatorTokenFile, "operator-token-file", "", "")
		f.StringVar(&c.controlPlane, "control-plane", c.controlPlane, "")
	}
	if c.verb == "mark" || c.verb == "load" {
		f.StringVar(&c.agentTokenFile, "agent-token-file", "", "")
	}
	if c.verb == "mark" || c.verb == "load" || c.verb == "measure" {
		f.StringVar(&c.expectCell, "expect-cell", "", "")
	}
	if c.verb == "mark" || c.verb == "close" {
		f.BoolVar(&c.yes, "yes", false, "")
	}
	if c.verb == "check" || c.verb == "load" || c.verb == "measure" {
		f.StringVar(&c.kubeContext, "kube-context", "", "")
		f.StringVar(&c.kubeconfig, "kubeconfig", "", "")
		f.StringVar(&c.kubectl, "kubectl", c.kubectl, "")
		f.Float64Var(&c.limits.fs, "max-node-fs-percent", 75, "")
	}
	if c.verb == "check" || c.verb == "load" {
		f.Float64Var(&c.limits.memory, "max-node-memory-percent", 90, "")
		f.Float64Var(&c.limits.rise, "max-node-memory-rise", 5, "")
		f.Float64Var(&c.limits.pvc, "max-pvc-percent", 70, "")
	}
	var founder, entries string
	if c.verb == "load" {
		f.StringVar(&founder, "founder-archive-bytes", "", "")
		f.StringVar(&entries, "entries", "", "")
		f.BoolVar(&c.dryRun, "dry-run", false, "")
		f.BoolVar(&c.noMeasure, "no-measure", false, "")
		f.IntVar(&c.maxMeasures, "max-measures", 2, "")
		f.Int64Var(&c.rate, "max-write-bytes-per-second", 1000000, "")
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 {
		return c, failure(2, usage)
	}
	required := [][2]string{}
	switch c.verb {
	case "check":
		required = append(required, [2]string{"kube-context", c.kubeContext})
	default:
		required = append(required, [2]string{"account", c.account})
		if c.verb == "mark" || c.verb == "load" {
			required = append(required, [2]string{"agent-token-file", c.agentTokenFile})
		}
		required = append(required, [2]string{"operator-token-file", c.operatorTokenFile})
		if c.verb != "close" {
			required = append(required, [2]string{"expect-cell", c.expectCell})
		}
		if c.verb == "load" || c.verb == "measure" {
			required = append(required, [2]string{"kube-context", c.kubeContext})
		}
	}
	for _, v := range required {
		if v[1] == "" {
			return c, failure(2, "missing required flag --%s", v[0])
		}
	}
	if c.verb != "check" && !accountPattern.MatchString(c.account) {
		return c, failure(2, "--account must be an acc_ id")
	}
	if c.verb == "load" {
		seen := map[string]bool{}
		f.Visit(func(f *flag.Flag) { seen[f.Name] = true })
		if seen["founder-archive-bytes"] == seen["entries"] {
			return c, failure(2, "give exactly one of --founder-archive-bytes or --entries")
		}
		var err error
		if seen["founder-archive-bytes"] {
			c.founder, err = strconv.ParseInt(founder, 10, 64)
			if err != nil || c.founder <= 0 {
				return c, failure(2, "--founder-archive-bytes must be a positive integer")
			}
		}
		if seen["entries"] {
			c.entries, err = strconv.ParseInt(entries, 10, 64)
			if err != nil || c.entries < 1 || c.entries > 450000 {
				return c, failure(2, "--entries must be between 1 and 450000")
			}
		}
	}
	for _, v := range []struct {
		name             string
		value, low, high float64
	}{
		{"max-node-memory-percent", c.limits.memory, 50, 95}, {"max-node-memory-rise", c.limits.rise, 1, 10}, {"max-pvc-percent", c.limits.pvc, 30, 75}, {"max-node-fs-percent", c.limits.fs, 30, 78}, {"max-write-bytes-per-second", float64(c.rate), 100000, 2000000}, {"max-measures", float64(c.maxMeasures), 1, 2},
	} {
		if math.IsNaN(v.value) || math.IsInf(v.value, 0) || v.value < v.low || v.value > v.high {
			return c, failure(2, "--%s must be between %.0f and %.0f", v.name, v.low, v.high)
		}
	}
	return c, nil
}
func cliWithOptions(ctx context.Context, args []string, d deps, o options) int {
	c, err := parseConfig(args)
	if err == nil {
		if ctx.Err() != nil {
			err = failure(130, "stopped: interrupted")
		} else {
			switch c.verb {
			case "check":
				err = check(ctx, c, d)
			case "mark":
				err = mark(ctx, c, d, o)
			case "load":
				err = load(ctx, c, d, o)
			case "measure":
				err = measure(ctx, c, d, o)
			case "close":
				err = closeAccount(ctx, c, d, o)
			}
		}
	}
	if err == nil {
		return 0
	}
	if ctx.Err() != nil {
		err = failure(130, "stopped: interrupted")
	}
	var te *toolError
	if !errors.As(err, &te) {
		te = failure(5, "stopped: unexpected response")
	}
	_, _ = fmt.Fprintln(d.stderr, te.msg)
	return te.code
}
func check(ctx context.Context, c config, d deps) error {
	r, err := probe(ctx, c, d, false)
	if err != nil {
		return err
	}
	kv(d, "kube context:", safeText(c.kubeContext))
	kv(d, "nodes:", len(r.nodes))
	kv(d, "node memory:", fmt.Sprintf("%s (stop at %.1f%%)", nodePercentages(r, true), c.limits.memory))
	kv(d, "node filesystem:", fmt.Sprintf("%s (stop at %.1f%%)", nodePercentages(r, false), c.limits.fs))
	kv(d, "postgres volume:", fmt.Sprintf("%.1f%% of %d bytes (stop at %.1f%%)", r.pvc, r.pvcCapacity, c.limits.pvc))
	kv(d, "postgres restarts:", r.restarts)
	reason := r.lastReason
	if reason == "" {
		reason = "none"
	}
	kv(d, "postgres last exit:", reason)
	kv(d, "postgres ready:", r.ready)
	if err = evaluateReadings(r, r, c.limits, false); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(d.stdout, "ok")
	return nil
}
