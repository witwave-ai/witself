package localcell

// fixedCause is reserved for compile-time, source-safe diagnostics. Never
// convert command output, an underlying error, or an operator value to this type.
type fixedCause string

func (e fixedCause) Error() string { return string(e) }

// ErrLocalMigrationPhaseRequired refuses runtime migration authority.
var ErrLocalMigrationPhaseRequired = fixedCause("local_migration_phase_required")

// ErrLocalRuntimeLoginOwnsTables refuses unsafe runtime custody or identity.
var ErrLocalRuntimeLoginOwnsTables = fixedCause("local_runtime_login_owns_tables")

var errPrincipalLayout = fixedCause("local_principal_layout_invalid")
var errPrincipalProvision = fixedCause("local_principal_provision_refused")
var errPrincipalSessions = fixedCause("local_privileged_sessions_present")
var errPrincipalRepair = fixedCause("local_principal_repair_required")

// ErrLocalPrincipalCredentialsRefused refuses incomplete or unsafe private credentials.
var ErrLocalPrincipalCredentialsRefused = fixedCause("local_principal_credentials_refused")

var errMaintenanceFenceActive = fixedCause("local_maintenance_fence_active")
var errUpdatePartialState = fixedCause("local_update_partial_state")

// Reinstall refuses a root it cannot readmit, a different installation, or a
// different PostgreSQL major or schema than this build.
var errReinstallState = fixedCause("local_reinstall_state_unsupported")
var errReinstallIdentity = fixedCause("local_reinstall_identity_mismatch")
var errReinstallVersion = fixedCause("local_reinstall_version_mismatch")
