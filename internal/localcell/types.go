package localcell

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"
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
	Root      string
	Binaries  Binaries
	Jobs      []string
	Timeout   time.Duration
	Failpoint func(operation, step string) error
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
	AccountID                 string        `json:"account_id,omitempty"`
	LastVerifiedSchemaVersion int           `json:"last_verified_schema_version"`
	LastOperation             string        `json:"last_operation"`
	Installed                 bool          `json:"installed"`
	Cleanup                   *journalEntry `json:"cleanup,omitempty"`
}

// InstallOptions controls whether PostgreSQL stays running after installation.
type InstallOptions struct{ LeaveRunning bool }

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
}

// Status adds bounded, live readiness observations to Inspection.
type Status struct {
	Inspection
	ServerReady bool `json:"server_ready"`
	WorkerReady bool `json:"worker_ready"`
}

// Cell is a handle, not a daemon. Every operation rereads its durable state.
type Cell struct {
	cfg Config
}

// New validates configuration without creating anything or starting a process.
func New(cfg Config) (*Cell, error) {
	if cfg.Root == "" {
		return nil, errors.New("local cell root is required")
	}
	if err := validateRootPath(cfg.Root); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil || root == filepath.VolumeName(root)+string(os.PathSeparator) {
		return nil, errors.New("invalid local cell root")
	}
	cfg.Root = filepath.Clean(root)
	parent, err := filepath.EvalSymlinks(filepath.Dir(cfg.Root))
	if err != nil {
		return nil, errors.New("local cell parent directory is unavailable")
	}
	cfg.Root = filepath.Join(parent, filepath.Base(cfg.Root))
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
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

func localHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
