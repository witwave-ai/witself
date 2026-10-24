// Package localcell owns a disposable local cell under one caller-supplied root.
// Only tests and the internal localcellharness command use it; it does not
// install, activate, or change an ordinary Witself installation.
//
// The root contains manifest.json, pg/ for PostgreSQL data, spool/ for the local
// runtime archive spool, run/ for operation locking, process identity records,
// the PostgreSQL password file, journal/ for append-only
// operation records, logs/ for component output, tmp/ for private temporary
// files, and home/ for isolated child homes. Directories are created with mode
// 0700 and secret/state files with mode 0600 on Unix. A stable root .owner.lock
// also fences removal of run/: deleting a held lock inode must never allow a
// second owner to lock a replacement inode concurrently. It remains after
// removal. Windows objects require the current user as owner and a protected
// DACL with explicit full control only for that user and SYSTEM; reparse paths
// and non-local, non-NTFS, reserved or long roots are refused. Retained handles
// fence ancestor renames; pathname creation detects in-place reparse changes
// afterward and rolls back the created object by handle. This is detection with
// rollback, not prevention; handle-relative creation remains deferred.
//
// Install, Start, Stop, Repair, and Remove require exclusive OS advisory locks;
// a competing operation fails promptly. Inspect reads the manifest, process
// identities, data presence, lock state, and interrupted journal operation
// without changing them. Status adds bounded live readiness checks. Mutations
// record started, done, or failed journal steps and atomically replace the
// manifest. Repair uses those records to complete or reverse interrupted work;
// it never deletes pg/. Remove preserves pg/ and the manifest unless the caller
// explicitly requests purge.
//
// Component binaries are supplied by the caller. PostgreSQL and all service
// listeners bind to loopback, and server/worker environments are constructed
// from an explicit allowlist with private HOME and temporary directories.
// Parent credentials and hosted settings are never inherited. Start waits for
// database/schema/account readiness and rolls back components it started on
// failure. Stop verifies recorded process identity and PostgreSQL's recorded
// data directory before signaling anything; it refuses unrelated processes.
// Unix child startup persists its identity before an explicit handshake allows
// the component to execute. Final removal checkpoints journal intent in the
// manifest so Repair can complete cleanup after the journal directory is gone.
//
// Windows 10+ creates each actual executable suspended in an atomic kill-on-close
// Job Object, flushes its private creation-FILETIME record before resume, and
// transfers cleanup to that record only after launch confirmation; parent crashes
// before transfer kill the job and Repair reconciles its record. Exact trusted
// environments and retained-handle identity checks fence shutdown, with an
// isolated console attachment helper for server/worker CTRL_BREAK and an explicit
// refusal when their console no longer exists (no service or console-close resilience).
//
// Restart policy, migration cancellation, launchd/systemd/Windows service
// registration, downloads, the installer manifest trust contract, native Windows
// lifecycle proof, and supported local-product routing/authorization remain deferred.
package localcell
