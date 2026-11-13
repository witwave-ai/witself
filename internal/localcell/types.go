package localcell

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// LayoutVersion is the only on-disk layout understood by this implementation.
const LayoutVersion = 1

// localDatabaseName belongs to the disposable cluster, independent of shared
// PostgreSQL database names assigned to repository test lanes.
const localDatabaseName = "witself"

// Binaries are explicit caller-supplied executable locations. Nothing is downloaded.
type Binaries struct {
	PostgresDir string
	Server      string
	Worker      string
}

// Config describes one disposable cell. Failpoint is a test-only crash seam:
// an error after a step leaves its journal open, without ordinary error rollback.
type Config struct {
	Root     string
	Binaries Binaries
	Jobs     []string
	Timeout  time.Duration
	// Zero values preserve the manifest settings, or the local runtime defaults
	// for older manifests that predate these fields.
	MigrationWait    time.Duration
	JobRestartPolicy string
	Failpoint        func(operation, step string) error
}

// Ports are loopback TCP listeners allocated for this cell.
type Ports struct {
	Postgres      int `json:"postgres"`
	ServerAPI     int `json:"server_api"`
	ServerHealth  int `json:"server_health"`
	ServerMetrics int `json:"server_metrics"`
	WorkerHealth  int `json:"worker_health"`
	WorkerMetrics int `json:"worker_metrics"`
}

// Manifest contains no password or connection URL.
type Manifest struct {
	LayoutVersion             int           `json:"layout_version"`
	InstallationID            string        `json:"installation_id"`
	CreatedAt                 time.Time     `json:"created_at"`
	PostgreSQLVersion         string        `json:"postgresql_version"`
	ServerVersion             string        `json:"server_version"`
	WorkerVersion             string        `json:"worker_version"`
	Ports                     Ports         `json:"ports"`
	Database                  string        `json:"database"`
	Role                      string        `json:"role"`
	PrincipalLayoutVersion    int           `json:"principal_layout_version,omitempty"`
	PrincipalRoles            []string      `json:"principal_roles,omitempty"`
	AccountID                 string        `json:"account_id,omitempty"`
	LastVerifiedSchemaVersion int           `json:"last_verified_schema_version"`
	LastOperation             string        `json:"last_operation"`
	Installed                 bool          `json:"installed"`
	RemovalRetained           bool          `json:"removal_retained,omitempty"`
	Cleanup                   *journalEntry `json:"cleanup,omitempty"`
	MigrationWait             time.Duration `json:"migration_wait,omitempty"`
	JobRestartPolicy          string        `json:"job_restart_policy,omitempty"`
}

// updateState names a durable boundary, not proof that the action completed.
type updateState string

// update protocol states are journaled before their associated actions.
const (
	updatePrepared             updateState = "prepared"
	updateMaintenanceRequested updateState = "maintenance_requested"
	updateAcknowledged         updateState = "acknowledged"
	updateQuiesced             updateState = "quiesced"
	updateRecoveryVerified     updateState = "recovery_verified"
	updateMigrating            updateState = "migrating"
	updateActivated            updateState = "activated"
	updateHealthVerified       updateState = "health_verified"
	updateCommitted            updateState = "committed"
)

// valid accepts exactly the nine journal steps in the update protocol.
func (s updateState) valid() bool {
	switch s {
	case updatePrepared, updateMaintenanceRequested, updateAcknowledged, updateQuiesced,
		updateRecoveryVerified, updateMigrating, updateActivated, updateHealthVerified, updateCommitted:
		return true
	default:
		return false
	}
}

// updateComponentSetIdentity preserves the private trust receipt wire shape
// without enrolling the offline trust package in server/worker dependencies.
type updateComponentSetIdentity struct {
	ManifestDigest string `json:"manifest_digest"`
	Channel        string `json:"channel"`
	Sequence       uint64 `json:"sequence"`
	Tag            string `json:"tag"`
}

// updateRecord is private durable metadata. Its state is derived from the journal;
// the identities must never be copied into diagnostic evidence.
type updateRecord struct {
	OperationID           string                     `json:"operation_id"`
	InstallationID        string                     `json:"installation_id"`
	MaintenanceGeneration int                        `json:"maintenance_generation"`
	Source                updateComponentSetIdentity `json:"source"`
	Target                updateComponentSetIdentity `json:"target"`
	Channel               string                     `json:"channel"`
	ChannelAction         string                     `json:"channel_action,omitempty"`
	SchemaFrom            int                        `json:"schema_from"`
	SchemaTo              int                        `json:"schema_to"`
	LayoutVersion         int                        `json:"layout_version"`
	PGMajor               int                        `json:"pg_major"`
	RecoveryPoint         string                     `json:"recovery_point"`
	State                 updateState                `json:"state"`
	AckedGeneration       int                        `json:"acked_generation"`
	RetainedGenerations   int                        `json:"retained_generations"`
}

// InstallOptions controls whether PostgreSQL stays running after installation.
type InstallOptions struct {
	LeaveRunning      bool
	ManagedPrincipals bool
}

// RemoveOptions requires explicit consent before deleting retained database data.
type RemoveOptions struct{ Purge bool }

// ProcessState reports observation without granting authority to signal a PID.
type ProcessState struct {
	PID     int  `json:"pid"`
	Alive   bool `json:"alive"`
	Matches bool `json:"matches"`
}

// Interruption identifies the operation and its last recorded step.
type Interruption struct {
	Operation string `json:"operation"`
	Step      string `json:"step"`
}

// Inspection is a read-only snapshot. Locked snapshots may change concurrently.
type Inspection struct {
	Manifest              *Manifest               `json:"manifest,omitempty"`
	Locked                bool                    `json:"locked"`
	Processes             map[string]ProcessState `json:"processes"`
	PGDataPresent         bool                    `json:"pg_data_present"`
	Interrupted           *Interruption           `json:"interrupted,omitempty"`
	JournalTailIncomplete bool                    `json:"journal_tail_incomplete,omitempty"`
	InstallState          string                  `json:"install_state,omitempty"`
	PrincipalTopology     string                  `json:"principal_topology,omitempty"`
}

// Status adds bounded, live readiness observations to Inspection.
type Status struct {
	Inspection
	ServerReady bool `json:"server_ready"`
	WorkerReady bool `json:"worker_ready"`
}

// Cell is a handle, not a daemon. Every operation rereads its durable state.
type Cell struct {
	// Only an explicitly started Windows control host populates this state.
	serviceControlMu       sync.Mutex
	serviceControlOwned    *serviceControlEndpointRecord
	serviceControlListener net.Listener //nolint:unused // Windows owns listener lifetime; Unix has no endpoint record.
	cfg                    Config
	childrenMu             sync.Mutex
	children               map[string]*childObservation
	// Test failpoint between releasing an allocation and probing it. Nil in production.
	afterPortAllocation func(Ports)
	// Test observers scoped to interrupted PostgreSQL recovery. Nil in production.
	recoveryProcessIdentity  func(int) (string, bool, error)
	recoveryProcessStartedAt func(int) (time.Time, error)
	installerPrincipals      *installerPrincipalOperations
	installerStart           *installerStartOperations
	maintenanceTest          *installerMaintenanceOperations
	reinstallTest            *reinstallOperations
}

// CanonicalRoot makes two spellings of one directory canonicalize to the same string.
func CanonicalRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("local cell root is required")
	}
	if err := validateRootPath(root); err != nil {
		return "", err
	}
	root, err := filepath.Abs(root)
	if err != nil || root == filepath.VolumeName(root)+string(os.PathSeparator) {
		return "", errors.New("invalid local cell root")
	}
	root = filepath.Clean(root)
	parent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		return "", errors.New("local cell parent directory is unavailable")
	}
	return filepath.Join(parent, filepath.Base(root)), nil
}

// New validates configuration without creating anything or starting a process.
func New(cfg Config) (*Cell, error) {
	root, err := CanonicalRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	cfg.Root = root
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if err := validateRuntimeSettings(cfg.MigrationWait, cfg.JobRestartPolicy); err != nil {
		return nil, err
	}
	allowed := map[string]bool{"avatar_style_rollout": true, "message_rate_bucket_cleanup": true, "agent_email_rate_bucket_cleanup": true, "transcript_storage_reconcile": true}
	for _, job := range cfg.Jobs {
		if !allowed[job] {
			return nil, errors.New("local cell job is not allowlisted")
		}
	}
	for _, binary := range []string{cfg.Binaries.PostgresDir, cfg.Binaries.Server, cfg.Binaries.Worker} {
		if binary != "" && !filepath.IsAbs(binary) {
			return nil, errors.New("local cell binary paths must be absolute")
		}
	}
	return &Cell{cfg: cfg}, nil
}

func (c *Cell) path(parts ...string) string {
	return filepath.Join(append([]string{c.cfg.Root}, parts...)...)
}

func (c *Cell) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.cfg.Timeout)
}

func validateRuntimeSettings(wait time.Duration, policy string) error {
	if wait < 0 {
		return errors.New("local cell migration wait must be positive")
	}
	if policy != "" && policy != runtimeconfig.JobRestartNever && policy != runtimeconfig.JobRestartOnFailure {
		return errors.New("local cell job restart policy is unsupported")
	}
	return nil
}

func (c *Cell) runtimeSettings(m *Manifest) (time.Duration, string) {
	wait, policy := m.MigrationWait, m.JobRestartPolicy
	if c.cfg.MigrationWait != 0 {
		wait = c.cfg.MigrationWait
	}
	if c.cfg.JobRestartPolicy != "" {
		policy = c.cfg.JobRestartPolicy
	}
	if wait == 0 {
		wait = runtimeconfig.DefaultMigrationWait
	}
	if policy == "" {
		policy = runtimeconfig.JobRestartNever
	}
	return wait, policy
}

func (c *Cell) readinessTimeout(component string, m *Manifest) time.Duration {
	if component != "server" {
		return c.cfg.Timeout
	}
	wait, _ := c.runtimeSettings(m)
	// Saturate instead of wrapping an otherwise valid large positive duration.
	const maxDuration = time.Duration(1<<63 - 1)
	if wait > maxDuration-c.cfg.Timeout {
		return maxDuration
	}
	return c.cfg.Timeout + wait
}

func localHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
