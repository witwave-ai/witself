package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// selfExportSnapshotTimeout bounds the database phase of a self export. The
// snapshot is spooled server-side, so this is pure database time — client
// pacing never extends it.
const selfExportSnapshotTimeout = 15 * time.Minute

var (
	// ErrVaultLifecycleInProgress signals that an account's sealed-plane key
	// enrollment or rotation is not yet in an archive-portable state.
	ErrVaultLifecycleInProgress = errors.New("vault lifecycle in progress")
	// ErrExportSchemaAhead signals that this server binary cannot faithfully
	// describe the database schema it is connected to.
	ErrExportSchemaAhead = errors.New("export schema is ahead of server")
)

// accountSelfExportHandler exposes the authenticated account as a streaming
// customer archive. Operator tokens are the cell's existing account-wide
// customer credential; using that boundary avoids treating a realm-scoped
// agent token as authority over sibling realms.
func accountSelfExportHandler(
	auth AuthFunc,
	stream func(context.Context, string, io.Writer) error,
	reportFailure func(context.Context, string, error),
) http.HandlerFunc {
	var active struct {
		sync.Mutex
		accounts map[string]struct{}
	}
	active.accounts = make(map[string]struct{})

	return requireOperatorAnyStatus(auth, func(
		w http.ResponseWriter,
		r *http.Request,
		p principal,
	) {
		active.Lock()
		if _, exists := active.accounts[p.accountID]; exists {
			active.Unlock()
			writeSelfExportError(
				w,
				http.StatusConflict,
				"account_export_in_progress",
				"an export is already in progress for this account",
			)
			return
		}
		active.accounts[p.accountID] = struct{}{}
		active.Unlock()
		defer func() {
			active.Lock()
			delete(active.accounts, p.accountID)
			active.Unlock()
		}()

		// Spool the snapshot to a server-side temp file first so the
		// repeatable-read transaction closes at database speed. A customer
		// pacing (or stalling) the response body must never hold a snapshot
		// open on the shared cell database.
		spool, err := os.CreateTemp("", "witself-self-export-*.tar.gz")
		if err != nil {
			writeSelfExportError(
				w,
				http.StatusInternalServerError,
				"account_export_failed",
				"could not export account",
			)
			return
		}
		spoolPath := spool.Name()
		defer func() {
			_ = spool.Close()
			_ = os.Remove(spoolPath)
		}()

		snapshotCtx, cancel := context.WithTimeout(
			r.Context(), selfExportSnapshotTimeout,
		)
		err = stream(snapshotCtx, p.accountID, spool)
		cancel()
		if err == nil {
			_, err = spool.Seek(0, io.SeekStart)
		}
		var size int64
		if err == nil {
			info, statErr := spool.Stat()
			if statErr != nil {
				err = statErr
			} else {
				size = info.Size()
			}
		}
		if err == nil {
			filename := fmt.Sprintf(
				"witself-export-%s-%s.tar.gz",
				p.accountID,
				time.Now().UTC().Format("20060102"),
			)
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("X-Witself-Export-Format", "1")
			w.Header().Set("X-Witself-Export-Purpose", "self")
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set(
				"Content-Disposition",
				fmt.Sprintf("attachment; filename=%q", filename),
			)
			if _, copyErr := io.Copy(w, spool); copyErr != nil {
				// The transaction is long closed; a client that hangs up
				// mid-download costs only this handler.
				if reportFailure != nil {
					reportFailure(r.Context(), p.accountID, copyErr)
				}
			}
			return
		}
		if reportFailure != nil {
			reportFailure(r.Context(), p.accountID, err)
		}

		switch {
		case errors.Is(err, ErrVaultLifecycleInProgress):
			writeSelfExportError(
				w,
				http.StatusConflict,
				"vault_lifecycle_in_progress",
				"finish or cancel the open vault enrollment or key rotation, then retry the export",
			)
		case errors.Is(err, ErrExportSchemaAhead):
			writeSelfExportError(
				w,
				http.StatusServiceUnavailable,
				"export_schema_ahead",
				"this cell must be upgraded before the account can be exported",
			)
		default:
			writeSelfExportError(
				w,
				http.StatusInternalServerError,
				"account_export_failed",
				"could not export account",
			)
		}
	})
}

func writeSelfExportError(
	w http.ResponseWriter,
	status int,
	code, message string,
) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Content-Disposition")
	w.Header().Del("X-Witself-Export-Format")
	w.Header().Del("X-Witself-Export-Purpose")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0",
		"code":           code,
		"error":          message,
		"retryable":      status >= http.StatusInternalServerError,
	})
}
