package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Retain the phase without exposing the driver's error string or connection data.
type capturePoolAcquireError struct{ error }

func (e capturePoolAcquireError) Unwrap() error { return e.error }

func captureObservationFailure(err error) string {
	return "capture lock observation failed class=" + captureObservationClass(err)
}

func captureObservationClass(err error) string {
	phase := ""
	var acquire capturePoolAcquireError
	if errors.As(err, &acquire) {
		phase = "pool-acquire/"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return phase + "deadline"
	case errors.Is(err, context.Canceled):
		return phase + "canceled"
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr != nil && len(pgerr.Code) == 5 && strings.Trim(pgerr.Code, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
		return phase + "sqlstate=" + pgerr.Code
	}
	return phase + "other"
}

type captureObservationReporter interface {
	Helper()
	Fatal(...any)
}

// Both pool and dedicated-connection observers use this loop so hermetic tests
// exercise the diagnostic emitted by an actual failed observation.
func captureObserve(ctx context.Context, t captureObservationReporter, query func(context.Context) (bool, error)) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := query(ctx)
		if err != nil {
			t.Fatal(captureObservationFailure(err))
			return
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(captureObservationFailure(ctx.Err()))
			return
		case <-ticker.C:
		}
	}
}

func capturePoolQuery(st *Store, query string, args ...any) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		conn, err := st.pool.Acquire(ctx)
		if err != nil {
			return false, capturePoolAcquireError{err}
		}
		defer conn.Release()
		var ok bool
		err = conn.QueryRow(ctx, query, args...).Scan(&ok)
		return ok, err
	}
}

type captureObservationStop struct{}

type captureObservationRecorder struct {
	diagnostic string
}

func (*captureObservationRecorder) Helper() {}

func (r *captureObservationRecorder) Fatal(args ...any) {
	r.diagnostic = fmt.Sprint(args...)
	panic(captureObservationStop{})
}

func recordedCaptureObservation(ctx context.Context, t *testing.T, query func(context.Context) (bool, error)) (diagnostic string, stopped bool) {
	t.Helper()
	recorder := &captureObservationRecorder{}
	defer func() {
		if value := recover(); value != nil {
			if _, ok := value.(captureObservationStop); !ok {
				t.Fatal("observation recorder encountered an unexpected panic")
			}
			diagnostic, stopped = recorder.diagnostic, true
		}
	}()
	captureObserve(ctx, recorder, query)
	return recorder.diagnostic, false
}

func TestSyncCaptureObservationClassification(t *testing.T) {
	private := "synthetic-private-query-dsn-pid"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "deadline"},
		{"canceled", fmt.Errorf("%s: %w", private, context.Canceled), "canceled"},
		{"postgres", &pgconn.PgError{Code: "42P01", Message: private, Detail: private}, "sqlstate=42P01"},
		{"wrapped_postgres", fmt.Errorf("%s: %w", private, &pgconn.PgError{Code: "40P01", Message: private}), "sqlstate=40P01"},
		{"server_cancel", &pgconn.PgError{Code: "57014", Message: private}, "sqlstate=57014"},
		{"pool_deadline", capturePoolAcquireError{context.DeadlineExceeded}, "pool-acquire/deadline"},
		{"pool_canceled", capturePoolAcquireError{context.Canceled}, "pool-acquire/canceled"},
		{"pool_connect", capturePoolAcquireError{&pgconn.PgError{Code: "53300", Message: private}}, "pool-acquire/sqlstate=53300"},
		{"pool_other", capturePoolAcquireError{errors.New(private)}, "pool-acquire/other"},
		{"other", errors.New(private), "other"},
		{"invalid_code", &pgconn.PgError{Code: private, Message: private}, "other"},
		{"invalid_five_bytes", &pgconn.PgError{Code: "x\n!?x", Message: private}, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			diagnostic, stopped := recordedCaptureObservation(context.Background(), t, func(context.Context) (bool, error) {
				calls++
				return false, tc.err
			})
			if !stopped || calls != 1 || diagnostic != "capture lock observation failed class="+tc.want {
				t.Fatal("observation diagnostic lost classification or exposed error data")
			}
		})
	}
}

func TestSyncCaptureObservationContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}},
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			calls := 0
			diagnostic, stopped := recordedCaptureObservation(ctx, t, func(context.Context) (bool, error) {
				calls++
				return false, nil
			})
			if !stopped || calls < 1 || diagnostic != "capture lock observation failed class="+tc.name {
				t.Fatal("observation context exit lost classification")
			}
		})
	}
}

func TestSyncCaptureObservationWaitsForPredicate(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	diagnostic, stopped := recordedCaptureObservation(ctx, t, func(context.Context) (bool, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return calls == 2, nil
	})
	if stopped || diagnostic != "" || calls != 2 {
		t.Fatal("observation returned before the predicate became true")
	}
}
