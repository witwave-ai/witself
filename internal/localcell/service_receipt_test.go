//go:build ownerreceipt && linux

package localcell_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/store"
)

// TestOwnerRebootReceipt is deliberately absent from all default builds. It
// leaves phase 1's service and private proof bearer alive across an actual reboot;
// neither a test cleanup nor phase 2 may start that service on the owner's behalf.
func TestOwnerRebootReceipt(t *testing.T) {
	phase := os.Getenv("WITSELF_RECEIPT_PHASE")
	if phase != "1" && phase != "2" && phase != "cleanup" {
		t.Fatal("usage: WITSELF_RECEIPT_PHASE=1|2|cleanup WITSELF_RECEIPT_ROOT=<absolute-persistent-root> WITSELF_LOCALCELL_PGBIN=<PostgreSQL-16-bin>")
	}
	TestOwnerReceiptRegressionGuards(t)
	root := ownerReceiptRoot(t, os.Getenv("WITSELF_RECEIPT_ROOT"))
	pgbin := os.Getenv("WITSELF_LOCALCELL_PGBIN")
	if !filepath.IsAbs(pgbin) || !postgresBinariesPresent(pgbin) {
		t.Fatal("receipt requires an absolute PostgreSQL 16 binary directory")
	}
	if os.Geteuid() == 0 {
		t.Fatal("receipt requires the owning non-root login user")
	}
	if phase == "1" {
		for _, name := range []string{"manifest.json", "receipt", "service", ".receipt-build", "home"} {
			if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("phase 1 requires a fresh root; existing receipt or installation refused")
			}
		}
		ownerReceiptDirectory(t, root, true)
		ownerReceiptDirectory(t, filepath.Join(root, "receipt"), true)
	} else {
		ownerReceiptDirectory(t, root, false)
		ownerReceiptDirectory(t, filepath.Join(root, "receipt"), false)
	}
	// These are dedicated disposable homes, not the owning user's real Witself
	// configuration. HOME stays unchanged so systemd finds the login user's unit.
	t.Setenv("WITSELF_HOME", filepath.Join(root, "home", "witself"))
	t.Setenv("DSH_HOME", filepath.Join(root, "home", "dsh"))
	t.Setenv("WITSELF_RUNTIME", "local")
	binaries := localcell.Binaries{PostgresDir: pgbin,
		Server: filepath.Join(root, ".receipt-build", lifecycleExecutableName("witself-server")),
		Worker: filepath.Join(root, ".receipt-build", lifecycleExecutableName("witself-worker"))}
	cell, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: 90 * time.Second})
	if err != nil {
		t.Fatal("cannot construct receipt cell")
	}
	host := filepath.Join(root, ".receipt-build", lifecycleExecutableName("localcellharness"))
	switch phase {
	case "1":
		ownerReceiptInstall(t, cell, root, host, binaries)
	case "2":
		ownerReceiptVerify(t, cell, root, host)
	case "cleanup":
		ownerReceiptCleanup(t, cell, root)
	}
}

func ownerReceiptRoot(t *testing.T, root string) string {
	t.Helper()
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || strings.ContainsAny(root, "\x00\r\n") || len(filepath.Join(root, "service", "control.sock")) >= 108 {
		t.Fatal("receipt root must be a short absolute persistent directory")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil || parent != filepath.Dir(root) {
		t.Fatal("receipt root requires an existing parent without symlink aliases")
	}
	for _, temporary := range []string{"/tmp", "/var/tmp", "/run", "/dev/shm", os.TempDir()} {
		temporary, err = filepath.Abs(temporary)
		if err != nil {
			t.Fatal("cannot resolve receipt temporary-directory exclusion")
		}
		for _, base := range []string{filepath.Clean(temporary), ownerReceiptResolved(temporary)} {
			if base != "" && (root == base || strings.HasPrefix(root, base+"/")) {
				t.Fatal("receipt root must survive reboot; temporary locations refused")
			}
		}
	}
	storage := parent
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatal("receipt root must be a real directory")
		}
		storage = root
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cannot inspect receipt root")
	}
	var filesystem syscall.Statfs_t
	if syscall.Statfs(storage, &filesystem) != nil {
		t.Fatal("cannot verify receipt persistent storage")
	}
	filesystemType := uint64(filesystem.Type) & 0xffffffff
	if filesystemType == 0x01021994 || filesystemType == 0x858458f6 { // tmpfs or ramfs
		t.Fatal("receipt root requires persistent storage")
	}
	return root
}

func ownerReceiptResolved(path string) string {
	resolved, _ := filepath.EvalSymlinks(path)
	return resolved
}

func ownerReceiptDirectory(t *testing.T, path string, create bool) {
	t.Helper()
	if create {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			t.Fatal("cannot create private receipt directory")
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownerReceiptOwned(info) {
		t.Fatal("receipt directory must be private and owned by the login user")
	}
}

func ownerReceiptOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func ownerReceiptInstall(t *testing.T, cell *localcell.Cell, root, host string, binaries localcell.Binaries) {
	t.Helper()
	buildDir := filepath.Join(root, ".receipt-build")
	ownerReceiptDirectory(t, buildDir, true)
	ownerReceiptDirectory(t, filepath.Join(buildDir, "tmp"), true)
	// Preserve the shared helper unchanged, including its private t.TempDir.
	// Stage under the persistent root and publish copies before that cleanup.
	t.Setenv("TMPDIR", filepath.Join(buildDir, "tmp"))
	built := buildRealComponents(t, binaries.PostgresDir)
	ownerReceiptCopy(t, built.Server, binaries.Server)
	ownerReceiptCopy(t, built.Worker, binaries.Worker)
	build := offlineBuildCommand(t.Context(), "build", "-o", host, "../tools/localcellharness")
	build.Stdout, build.Stderr = io.Discard, io.Discard
	if build.Run() != nil {
		t.Fatal("cannot build persistent receipt host")
	}
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatal("cannot install receipt cell")
	}
	descriptor := localcell.ServiceDescriptor{Adapter: "systemd-user", Scope: "user", Runtime: "local", DesiredState: "running", ResourceProfile: "unconfigured",
		Payload: localcell.ServicePayload{Host: host, Args: []string{"run", "--root", root, "--pgbin", binaries.PostgresDir, "--server", binaries.Server, "--worker", binaries.Worker, "--timeout", "60s"}}}
	if cell.PrepareService(t.Context(), descriptor) != nil || !ownerReceiptInvoke(t.Context(), host, root, "install") {
		t.Fatal("cannot register receipt user service")
	}
	registeredAt := time.Now().UTC()
	waitServiceReady(t, cell)
	before := nativeServiceProcess(t, root)
	manifest := ownerReceiptManifest(t, cell)
	bearer, operatorID := ownerReceiptWriteWitness(t, root, manifest)
	ownerReceiptWrite(t, filepath.Join(root, "receipt", "credential"), []byte(bearer))
	record := ownerReceiptPhase1{BootID: ownerReceiptBoot(t), ServerPID: before.PID, ServerStartTime: before.StartTime, OperatorID: operatorID, RegisteredAt: registeredAt}
	ownerReceiptWriteJSON(t, filepath.Join(root, "receipt", "phase1.json"), record)
	t.Log("RECEIPT PHASE1 OK — reboot the VM, log in as the same user, run phase 2")
}

func ownerReceiptCopy(t *testing.T, source, target string) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal("cannot open built receipt component")
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal("cannot publish receipt component")
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		t.Fatal("cannot persist receipt component")
	}
}

func ownerReceiptWriteWitness(t *testing.T, root string, manifest *localcell.Manifest) (string, string) {
	t.Helper()
	// Same credential mint and authenticated Unicode POST as
	// nativeServiceWriteWitness; retain only the bearer and the nonsecret row ID.
	db, err := store.Open(t.Context(), lifecycleDSN(t, root, manifest))
	if err != nil {
		t.Fatal("cannot open receipt proof database")
	}
	defer db.Close()
	owner, err := db.EnsureRootOperator(t.Context(), manifest.AccountID)
	if err != nil {
		t.Fatal("cannot resolve receipt proof operator")
	}
	ttl := time.Hour
	bearer, _, _, err := db.CreateOperatorToken(t.Context(), manifest.AccountID, owner, "service persistence proof", &ttl)
	if err != nil {
		t.Fatal("cannot mint receipt proof credential")
	}
	body, err := json.Marshal(map[string]string{"display_name": lifecycleUnicodeName, "ttl": "1h"})
	if err != nil {
		t.Fatal("cannot encode receipt proof write")
	}
	var created struct {
		Operator struct {
			ID string `json:"id"`
		} `json:"operator"`
	}
	lifecycleJSON(t, manifest, bearer, http.MethodPost, "/v1/operators", body, &created)
	if created.Operator.ID == "" {
		t.Fatal("receipt authenticated write omitted identity")
	}
	return bearer, created.Operator.ID
}

func ownerReceiptVerify(t *testing.T, cell *localcell.Cell, root, host string) {
	t.Helper()
	var before ownerReceiptPhase1
	ownerReceiptReadJSON(t, filepath.Join(root, "receipt", "phase1.json"), &before)
	if before.BootID == "" || before.ServerPID <= 1 || before.ServerStartTime == "" || before.OperatorID == "" || before.RegisteredAt.IsZero() {
		t.Fatal("invalid phase 1 receipt")
	}
	resultPath := filepath.Join(root, "receipt", "phase2.json")
	if _, err := os.Lstat(resultPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("phase 2 receipt already exists; preserve this attempt")
	}
	var descriptor localcell.ServiceDescriptor
	var records []localcell.ProcessRecord
	record, err := ownerReceiptRunPhase2(before, ownerReceiptOperations{
		boot:    func() string { return ownerReceiptBoot(t) },
		process: func() localcell.ProcessRecord { return nativeServiceProcess(t, root) },
		operators: func() []ownerReceiptOperator {
			bearer := ownerReceiptRead(t, filepath.Join(root, "receipt", "credential"))
			var listed struct {
				Operators []ownerReceiptOperator `json:"operators"`
			}
			lifecycleJSON(t, ownerReceiptManifest(t, cell), string(bearer), http.MethodGet, "/v1/operators", nil, &listed)
			return listed.Operators
		},
		captureProcesses: func() bool {
			var err error
			descriptor, err = cell.ServiceDescriptor()
			if err != nil {
				return false
			}
			records = ownerReceiptProcesses(t, root, descriptor)
			return true
		},
		operation: func(action string) bool {
			switch action {
			case "observe":
				return ownerReceiptObserve(t, cell)
			case "stop":
				status, err := cell.Control(t.Context(), "stop")
				if err != nil || status.DesiredState != "stopped" || status.Healthy {
					return false
				}
				// Capture the inventory only after every recorded child is stopped.
				for _, process := range records {
					if process.Component == "service-host" {
						continue
					}
					if alive, err := localcell.RecordAliveForTest(process); err != nil || alive {
						return false
					}
				}
				return true
			case "remove":
				return ownerReceiptInvoke(t.Context(), host, root, action)
			default:
				return false
			}
		},
		inventory: func() (map[string]ownerReceiptEntry, error) {
			return ownerReceiptInventory(filepath.Join(root, "pg"))
		},
		retained: func() bool {
			if !ownerReceiptUnregistered(t.Context(), descriptor) {
				return false
			}
			inspection, err := cell.Inspect(t.Context())
			if err != nil || inspection.Manifest == nil || !inspection.Manifest.Installed || !inspection.PGDataPresent {
				return false
			}
			if version, err := os.Lstat(filepath.Join(root, "pg", "PG_VERSION")); err != nil || !version.Mode().IsRegular() {
				return false
			}
			for _, process := range records {
				if alive, err := localcell.RecordAliveForTest(process); err != nil || alive {
					return false
				}
			}
			for _, process := range inspection.Processes {
				if process.Alive && process.Matches {
					return false
				}
			}
			return true
		},
		deleteCredential: func() error { return os.Remove(filepath.Join(root, "receipt", "credential")) },
		publish: func(data []byte) error {
			ownerReceiptWrite(t, resultPath, data)
			return nil
		},
	})
	if err != nil {
		ownerReceiptWriteJSON(t, resultPath, record)
		t.Fatal(err.Error()) // Only fixed contract/proof names cross this boundary.
	}
	t.Log("RECEIPT PHASE2 OK")
}

func ownerReceiptObserve(t *testing.T, cell *localcell.Cell) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Observe returns the cached authenticated supervisor snapshot; it cannot
		// step, start, repair or run the lifecycle on the receipt's behalf.
		if status, err := cell.Control(ctx, "observe"); err == nil && status.Healthy && status.LastStepOutcome == "ok" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func ownerReceiptProcesses(t *testing.T, root string, descriptor localcell.ServiceDescriptor) []localcell.ProcessRecord {
	t.Helper()
	var records []localcell.ProcessRecord
	for _, component := range []string{"postgres", "server", "worker"} {
		var record localcell.ProcessRecord
		ownerReceiptReadJSON(t, filepath.Join(root, "run", component+".json"), &record)
		if record.Root != root || record.Component != component || record.PID <= 1 || record.StartTime == "" {
			t.Fatal("cannot retain owned receipt process identity")
		}
		records = append(records, record)
	}
	var lease struct {
		PID            int    `json:"pid"`
		StartIdentity  string `json:"start_identity"`
		Owner          string `json:"owner"`
		InstallationID string `json:"installation_id"`
	}
	ownerReceiptReadJSON(t, filepath.Join(root, "service", "lease.json"), &lease)
	if lease.PID <= 1 || lease.StartIdentity == "" || lease.Owner != strconv.Itoa(os.Geteuid()) || lease.InstallationID != descriptor.InstallationID {
		t.Fatal("cannot retain receipt service host identity")
	}
	return append(records, localcell.ProcessRecord{PID: lease.PID, StartTime: lease.StartIdentity, Root: root, Component: "service-host"})
}

func ownerReceiptUnregistered(ctx context.Context, descriptor localcell.ServiceDescriptor) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	label := "witself-cell-" + descriptor.InstallationID + ".service"
	command := exec.CommandContext(ctx, "systemctl", "--user", "is-enabled", label)
	command.Env = append(os.Environ(), "LC_ALL=C")
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	output, err := command.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || ctx.Err() != nil {
		return false
	}
	state := strings.TrimSpace(string(output))
	// systemd versions differ: a deleted unit can be reported only on stderr.
	// Match the exact missing-file diagnostic, never a general command failure.
	missing := state == "" && exit.ExitCode() == 1 && strings.TrimSpace(diagnostic.String()) ==
		"Failed to get unit file state for "+label+": No such file or directory"
	if state != "disabled" && state != "not-found" && !missing {
		return false
	}
	observation, err := localcell.NewServiceAdapter().Inspect(ctx, descriptor)
	return err == nil && observation.Available && !observation.Registered
}

func ownerReceiptCleanup(t *testing.T, cell *localcell.Cell, root string) {
	t.Helper()
	descriptor, err := cell.ServiceDescriptor()
	if err == nil {
		// Cleanup may inspect the manager but never change files outside root.
		// Phase 2 (or the owner's explicit failure recovery) must unregister first.
		if !ownerReceiptUnregistered(t.Context(), descriptor) {
			t.Fatal("cleanup requires prior owned service removal; follow the runbook recovery step")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cannot verify receipt cleanup ownership")
	}
	leasePath := filepath.Join(root, "service", "lease.json")
	if _, err := os.Lstat(leasePath); err == nil {
		var lease struct {
			PID           int    `json:"pid"`
			StartIdentity string `json:"start_identity"`
		}
		ownerReceiptReadJSON(t, leasePath, &lease)
		if lease.PID <= 1 || lease.StartIdentity == "" {
			t.Fatal("cannot verify receipt host cleanup identity")
		}
		if alive, err := localcell.RecordAliveForTest(localcell.ProcessRecord{PID: lease.PID, StartTime: lease.StartIdentity}); err != nil || alive {
			t.Fatal("cleanup requires the receipt service host to be stopped")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cannot inspect receipt host cleanup identity")
	}
	if cell.Remove(t.Context(), localcell.RemoveOptions{Purge: true}) != nil {
		t.Fatal("cannot purge receipt cell")
	}
	if ownerReceiptCleanupArtifacts(root) != nil {
		t.Fatal("cannot remove private receipt artifacts")
	}
	t.Log("RECEIPT CLEANUP OK")
}

func ownerReceiptInvoke(ctx context.Context, host, root, action string) bool {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, host, "service", action, "--root", root)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return ownerReceiptInvokeAllowed(action, func() bool { return command.Run() == nil })
}

func ownerReceiptManifest(t *testing.T, cell *localcell.Cell) *localcell.Manifest {
	t.Helper()
	inspection, err := cell.Inspect(t.Context())
	if err != nil || inspection.Manifest == nil || !inspection.Manifest.Installed {
		t.Fatal("cannot inspect installed receipt manifest")
	}
	return inspection.Manifest
}

func ownerReceiptBoot(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(data)) == "" {
		t.Fatal("cannot read receipt boot identity")
	}
	return strings.TrimSpace(string(data))
}

func ownerReceiptRead(t *testing.T, path string) []byte {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownerReceiptOwned(info) || info.Size() > 1<<20 {
		t.Fatal("receipt record must be a bounded private owned file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read private receipt record")
	}
	return data
}

func ownerReceiptReadJSON(t *testing.T, path string, value any) {
	t.Helper()
	if json.Unmarshal(ownerReceiptRead(t, path), value) != nil {
		t.Fatal("invalid receipt record")
	}
}

func ownerReceiptWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal("cannot encode value-free receipt")
	}
	ownerReceiptWrite(t, path, append(data, '\n'))
}

func ownerReceiptWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := ownerReceiptWritePrivate(path, data, ownerReceiptOwned); err != nil {
		t.Fatal(err.Error())
	}
}
