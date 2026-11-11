package localcell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func serviceControlMaintenanceExchange(t *testing.T, action string, answer func(serviceCommand) serviceReply) (ServiceStatus, error) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	cell, commands := serviceHealthControlFixture(t, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case command := <-commands:
			command.reply <- answer(command)
		case <-ctx.Done():
		}
	}()
	status, err := cell.Control(ctx, action)
	cancel()
	<-done
	return status, err
}

func TestServiceControlMaintenanceWire(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, action := range []string{"start", "repair", "repair_pending", "status", "health", "stop"} {
		t.Run("real/"+action, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			serviceMaintenanceHostSeed(t, f, "idle")
			if action == "repair_pending" {
				f.s.pendingStartup = serviceStartupReconcile
			}
			commandAction := action
			if action == "repair_pending" {
				commandAction = "repair"
			}
			serviceMaintenanceHostPlant(t, f.cell, "Valid")
			var replied serviceReply
			status, err := serviceControlMaintenanceExchange(t, commandAction, func(command serviceCommand) serviceReply {
				replied.status, replied.err = f.s.command(command.ctx, command.action)
				return replied
			})
			switch commandAction {
			case "start", "repair":
				if err != errMaintenanceFenceActive || !errors.Is(err, errMaintenanceFenceActive) || err.Error() != "local_maintenance_fence_active" {
					t.Fatal("Real fenced action lost maintenance sentinel identity")
				}
				if !reflect.DeepEqual(status, ServiceStatus{}) {
					t.Fatal("Real fenced refusal returned a populated status")
				}
				if commandAction == "repair" {
					d, readErr := f.cell.ServiceDescriptor()
					if readErr != nil || d.DesiredState != "stopped" || d.RepairRevision != f.d.RepairRevision {
						t.Fatal("Real fenced repair changed repair revision or stopped intent")
					}
				}
			case "status":
				if err != nil || status.State != "maintenance" || status.Reason != errMaintenanceFenceActive.Error() || !reflect.DeepEqual(status, replied.status) {
					t.Fatal("Real fenced status did not decode maintenance projection")
				}
			case "health":
				if err != errServiceUnhealthy || !reflect.DeepEqual(status, ServiceStatus{}) {
					t.Fatal("Real fenced health lost the unhealthy refusal")
				}
			case "stop":
				if replied.err != nil || err != nil || !reflect.DeepEqual(status, replied.status) || errors.Is(err, errMaintenanceFenceActive) {
					t.Fatal("Real fenced stop did not preserve supervisor completion")
				}
			}
		})
	}

	other := errors.New("synthetic control failure")
	wrapped := fmt.Errorf("synthetic wrapper %w", errMaintenanceFenceActive)
	wantStatus := ServiceStatus{State: "stopped", DesiredState: "stopped", Generation: "synthetic-generation", LastStepOutcome: "ok", Restarts: 2}
	for _, action := range []string{"start", "repair", "stop", "status", "health"} {
		rows := []struct {
			name string
			err  error
			want error
		}{
			{"site_wrapper", serviceFailureAt(errMaintenanceFenceActive, "supervisor_command"), nil},
		}
		if action == "start" || action == "repair" {
			rows[0].want = errMaintenanceFenceActive
			rows = append(rows, []struct {
				name string
				err  error
				want error
			}{
				{"joined", errors.Join(errMaintenanceFenceActive, other), nil},
				{"wrapped", wrapped, nil},
				{"deferred_join", errors.Join(errServiceControlDeferred, wrapped), errServiceControlDeferred},
				{"cancelled", context.Canceled, errServiceControlDeferred},
				{"other", other, nil},
				{"success", nil, nil},
			}...)
		}
		for _, row := range rows {
			t.Run("synthetic/"+action+"/"+row.name, func(t *testing.T) {
				status, err := serviceControlMaintenanceExchange(t, action, func(serviceCommand) serviceReply {
					return serviceReply{status: wantStatus, err: row.err}
				})
				if row.err == nil {
					if err != nil || !reflect.DeepEqual(status, wantStatus) {
						t.Fatal("Successful wire reply changed the supplied status")
					}
					return
				}
				if !reflect.DeepEqual(status, ServiceStatus{}) {
					t.Fatal("Synthetic wire refusal returned a populated status")
				}
				if row.want != nil {
					if err != row.want || !errors.Is(err, row.want) {
						t.Fatal("Synthetic wire refusal lost the expected sentinel")
					}
				} else if err == nil || err.Error() != "localcell: service_control_action_failed" || errors.Is(err, errMaintenanceFenceActive) {
					t.Fatal("Synthetic wire refusal escaped the generic closed error")
				}
			})
		}
	}
}

type serviceControlMaintenanceReader struct {
	text       string
	bytes      int
	reads      int
	oneByte    bool
	finalEOF   bool
	finalError bool
	endless    bool
	noProgress bool
}

func (r *serviceControlMaintenanceReader) Read(p []byte) (int, error) {
	r.reads++
	if r.noProgress {
		if r.reads > 64 {
			return 0, io.ErrNoProgress
		}
		return 0, nil
	}
	if r.endless {
		n := copy(p, r.text)
		r.text = r.text[n:]
		for i := n; i < len(p); i++ {
			p[i] = 'x'
		}
		r.bytes += len(p)
		return len(p), nil
	}
	if r.text == "" {
		if r.finalError {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, io.EOF
	}
	if r.oneByte && len(p) > 1 {
		p = p[:1]
	}
	n := copy(p, r.text)
	r.text = r.text[n:]
	r.bytes += n
	if r.finalEOF && r.text == "" {
		return n, io.EOF
	}
	return n, nil
}

func TestServiceControlMaintenanceRefusalMatch(t *testing.T) {
	exact := errMaintenanceFenceActive.Error() + "\n"
	for _, action := range []string{"start", "repair"} {
		t.Run("exact/"+action, func(t *testing.T) {
			if !serviceControlMaintenanceRefusal(action, http.StatusConflict, strings.NewReader(exact)) {
				t.Fatal("Exact maintenance wire body did not match")
			}
		})
	}
	for _, action := range []string{"stop", "status", "health", "observe", "exit-host", "", "START"} {
		name := action
		if name == "" {
			name = "empty"
		}
		t.Run("action/"+name, func(t *testing.T) {
			if serviceControlMaintenanceRefusal(action, http.StatusConflict, strings.NewReader(exact)) {
				t.Fatal("Unlisted action accepted a maintenance wire body")
			}
		})
	}
	for _, status := range []int{200, 202, 400, 401, 408, 500, 503} {
		t.Run(fmt.Sprintf("status/%d", status), func(t *testing.T) {
			if serviceControlMaintenanceRefusal("start", status, strings.NewReader(exact)) {
				t.Fatal("Unlisted status accepted a maintenance wire body")
			}
		})
	}
	for _, row := range []struct{ name, body string }{
		{"empty", ""},
		{"no_newline", strings.TrimSuffix(exact, "\n")},
		{"crlf", strings.TrimSuffix(exact, "\n") + "\r\n"},
		{"two_newlines", exact + "\n"},
		{"trailing_byte", exact + "x"},
		{"leading_space", " " + exact},
		{"upper_case", strings.ToUpper(exact)},
		{"prefix", "localcell: " + exact},
		{"suffix", strings.TrimSuffix(exact, "\n") + ": private\n"},
		{"large", strings.Repeat("x", 4096)},
	} {
		t.Run("body/"+row.name, func(t *testing.T) {
			if serviceControlMaintenanceRefusal("start", http.StatusConflict, strings.NewReader(row.body)) {
				t.Fatal("Nonexact maintenance wire body matched the sentinel")
			}
		})
	}
	for _, row := range []struct {
		name   string
		reader serviceControlMaintenanceReader
		want   bool
	}{
		{"one_byte", serviceControlMaintenanceReader{text: exact, oneByte: true}, true},
		{"final_bytes_with_eof", serviceControlMaintenanceReader{text: exact, finalEOF: true}, true},
		{"exact_then_error", serviceControlMaintenanceReader{text: exact, finalError: true}, false},
		{"endless", serviceControlMaintenanceReader{text: exact, endless: true}, false},
		{"no_progress", serviceControlMaintenanceReader{noProgress: true}, false},
	} {
		t.Run("reader/"+row.name, func(t *testing.T) {
			if got := serviceControlMaintenanceRefusal("start", http.StatusConflict, &row.reader); got != row.want {
				t.Fatal("Reader maintenance match produced the wrong verdict")
			}
			if row.reader.bytes > len(exact)+8 {
				t.Fatal("Maintenance matching read beyond its bounded byte budget")
			}
			if row.reader.reads > 64 {
				t.Fatal("Maintenance matching exceeded its bounded read budget")
			}
		})
	}
}
