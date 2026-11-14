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
// removal. Windows control objects require the current user as owner and a
// protected DACL with explicit full control only for that user and SYSTEM.
// PGDATA alone grants that same private access through directory inheritance
// so PostgreSQL-created descendants are private before their first write.
// Reparse paths and non-local, non-NTFS, reserved or long roots are refused. Retained handles
// fence ancestor renames; pathname creation detects in-place reparse changes
// afterward and rolls back the created object by handle. This is detection with
// rollback, not prevention; handle-relative creation remains deferred.
//
// Install, Start, Stop, Repair, Remove, Reinstall, Checkpoint, and DeleteCheckpoint
// require exclusive OS advisory locks;
// a competing operation fails promptly. Inspect reads the manifest, process
// identities, data presence, lock state, and interrupted journal operation
// without changing them. Status adds bounded live readiness checks. Mutations
// record started, done, or failed journal steps and atomically replace the
// manifest. Repair uses those records to complete or reverse interrupted work;
// it never deletes pg/. Ordinary Remove preserves pg/, the manifest, all run/
// custody and journal/ evidence, and temporary spool/tmp/log material. After a
// verified stop it publishes removal_retained without clearing Installed, then
// closes the remove journal before clearing the manifest cleanup checkpoint.
// Repair completes an open removal the same way, whether it was interrupted or
// failed, and stops every recorded component before publishing the marker.
// Start refuses a marked cell before any journal record, credential read or
// launch (only its operation lock may be created first), and PostgreSQL boot
// refuses it independently; Install does not adopt the retained root. Only an
// explicit Reinstall naming the exact retained installation readmits it, for the
// same build and retained custody only: it starts PostgreSQL just to prove the
// custody the layout uses authenticates and the identity and schema match, stops
// it, and clears the marker last. It mints, rotates, migrates and deletes nothing.
// In the selected layout the bootstrap password is format-checked only: that
// layout never uses the bootstrap login after install, and its proof
// authenticates the six role credentials instead. A service in maintenance,
// Faulted or mid-transition refuses Reinstall, and a refusal may leave only the
// run/ lock entries that its lock prefix creates, as Start's does. Inspect offers
// reinstall only where Reinstall's offline admission predicate holds.
// Only a current explicit purge deletes the established owned cleanup targets.
// Repair never inherits purge authority or regenerates missing custody/history.
// Retained temporary files do not establish a durable pending-work guarantee.
//
// Checkpoint and DeleteCheckpoint are dark: no released command calls them.
// Checkpoint copies the PGDATA, manifest.json and retained custody of the cold
// installation named by its exact identity into a new recovery/<id>/, re-reads
// the copy, writes inventory.json, and writes witness.json last; only a
// witnessed checkpoint is complete. Before any write it proves the cell cold: no
// live component, no pg/postmaster.pid, and a pg_controldata state of shut down;
// before the witness it requires an equal second control reading, no
// pg/postmaster.pid and an unchanged pg_control; process liveness is not
// re-checked then. pg_controldata runs from Binaries.PostgresDir, so a
// bundle without it refuses every checkpoint. At most two complete checkpoints
// are kept; a new one needs free space of at least 2·S + 1 GiB for a copy of
// estimated size S. Only DeleteCheckpoint naming one exact identifier deletes
// one: purge, removal, Reinstall and Repair keep them. Checkpoints hold all data
// and every password, survive purge, and stay unverified until the D2.1-b2
// drill: complete means the writer finished, not that the data was verified.
// The inventory and witness record digests that a later check can use to detect
// accidental corruption or truncation, not a same-user rewrite. Nothing verifies
// a kept checkpoint yet, and Inspect can list a damaged one as complete.
// Windows transient refusals while publishing a copied file or reopening it
// for re-hashing use the shared 2 s retry budget and 25–200 ms backoff, bounded
// also by 10 s plus 100 ms per planned file in cumulative requested sleep and
// by context cancellation. Other failures remain single attempts.
// Ordinary operations validate only the recovery/ directory itself;
// Checkpoint and DeleteCheckpoint validate all of recovery/** and refuse a
// tree that fails, which must then be repaired by hand. Inspect validates and
// lists checkpoints only while holding the cell lock, reporting listed. It
// reports busy when another operation owns the cell, or unowned when no lock
// file exists and the recovery boundary cannot be shown absent; neither state
// means there are no checkpoints. With no lock file and an absent boundary it
// reports listed with no checkpoints and creates nothing. A private
// unknown entry there (for example a 0600 .DS_Store on Unix) blocks Checkpoint
// but permits Inspect and exact-id DeleteCheckpoint; a non-private entry
// (for example 0644 on Unix) makes both mutations and an owned Inspect refuse
// until repaired by hand.
// A new installation in a purged root inherits the old checkpoints, with their
// passwords, which count toward the limit. Drill,
// restore, rollback, update wiring and D9 remain pending; this is no M4/M5 or
// supported-offering claim.
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
// manifest. Repair can settle predecessor cleanup when the journal is absent,
// while preserving a surviving journal and avoiding duplicate completion lines.
//
// Windows 10+ creates each actual executable suspended in an atomic kill-on-close
// Job Object, flushes its private creation-FILETIME record before resume, and
// transfers cleanup to that record only after launch confirmation; parent crashes
// before transfer kill the job and Repair reconciles its record. Exact trusted
// environments and retained-handle identity checks fence shutdown, with an
// isolated console attachment helper for server/worker CTRL_BREAK and an explicit
// refusal when their console no longer exists (no service or console-close resilience).
// PostgreSQL launches disable administrative groups and privileges, retain the
// current user, and explicitly use that user's ownership and a private default
// object DACL; an elevated CI parent never makes PostgreSQL administrative.
// Launch hands the child's identity to the shared exit observer.
//
// LC-S2-R1 bounds cancellable migration waits and supports an explicit in-process
// worker job restart policy. Server readiness includes the configured migration
// wait; early child exits retain their exit status and a bounded value-free
// diagnostic in the failed journal step. No server or PostgreSQL process is
// supervised or restarted: platform service adapters own process supervision.
// launchd/systemd/Windows service registration, product downloads, the installer
// manifest trust contract, the Windows 11 receipt, and supported local-product
// routing/authorization remain deferred.
package localcell
