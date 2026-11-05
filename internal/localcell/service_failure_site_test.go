package localcell

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var serviceFailureSitesReviewed = []string{
	"host_config", "host_descriptor", "host_prepare", "host_persisted_config",
	"initial_descriptor", "lease_directory", "lease_record", "lease_write",
	"control_secret", "control_transport", "control_cancelled", "endpoint_descriptor", "endpoint_read", "endpoint_write", "endpoint_lease", "endpoint_remove",
	"supervisor_load", "supervisor_traversal", "supervisor_save", "supervisor_start", "supervisor_stop", "supervisor_repair", "supervisor_reconcile", "supervisor_health", "supervisor_command",
	"health_lock", "health_tree", "health_database", "health_credential", "health_probe", "health_ready",
	"startup_reconcile", "startup_repair", "startup_stop", "shutdown_repair", "shutdown_stop", "boot_binding", "descriptor_write",
}

func TestServiceFailureSiteVocabulary(t *testing.T) {
	source, err := os.ReadFile("service_failure_site.go")
	if err != nil {
		t.Fatal("cannot read service failure vocabulary")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "sites.go", source, 0)
	if err != nil {
		t.Fatal("cannot parse service failure vocabulary")
	}
	var cases []string
	fallback := ""
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "serviceFailureSite" {
			continue
		}
		if len(fn.Body.List) != 1 {
			t.Fatal("service site vocabulary has an unreviewed return path")
		}
		switchSite, ok := fn.Body.List[0].(*ast.SwitchStmt)
		if !ok || switchSite.Init != nil {
			t.Fatal("service site vocabulary lost its closed switch")
		}
		input, ok := switchSite.Tag.(*ast.Ident)
		if !ok || input.Name != "site" {
			t.Fatal("service site vocabulary switched on an unreviewed input")
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			branch, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			if len(branch.List) == 0 {
				if len(branch.Body) != 1 {
					t.Fatal("service site fallback changed")
				}
				ret, ok := branch.Body[0].(*ast.ReturnStmt)
				if !ok || len(ret.Results) != 1 {
					t.Fatal("service site fallback changed")
				}
				literal, ok := ret.Results[0].(*ast.BasicLit)
				if !ok {
					t.Fatal("service site fallback changed")
				}
				fallback, _ = strconv.Unquote(literal.Value)
			} else {
				if len(branch.Body) != 1 {
					t.Fatal("service site vocabulary has an unreviewed accepted return")
				}
				ret, ok := branch.Body[0].(*ast.ReturnStmt)
				if !ok || len(ret.Results) != 1 {
					t.Fatal("service site vocabulary has an unreviewed accepted return")
				}
				value, ok := ret.Results[0].(*ast.Ident)
				if !ok || value.Name != "site" {
					t.Fatal("service site vocabulary renamed an accepted site")
				}
			}
			for _, expr := range branch.List {
				literal, ok := expr.(*ast.BasicLit)
				if !ok {
					t.Fatal("service site has dynamic vocabulary")
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal("service site has invalid vocabulary")
				}
				cases = append(cases, value)
			}
			return true
		})
	}
	if !reflect.DeepEqual(cases, serviceFailureSitesReviewed) || fallback != "unknown" {
		t.Fatal("service failure site vocabulary changed")
	}
	for _, site := range append(append([]string{}, serviceFailureSitesReviewed...), "unknown") {
		if serviceFailureSite(site) != site {
			t.Fatal("service failure site lost reviewed spelling")
		}
		cause := errors.New("private Windows filesystem object required")
		wrapped := serviceFailureAt(cause, site)
		if !errors.Is(wrapped, cause) || wrapped.Error() != cause.Error() {
			t.Fatal("service site changed error identity or checker literal")
		}
		var out bytes.Buffer
		WriteServiceHostFailure(&out, wrapped, "host_config")
		if out.String() != "native service host site="+site+"\n" {
			t.Fatal("service site receipt changed")
		}
	}
	for _, forged := range []string{"health", "", "private/path", "endpoint-read", "site=lease_write"} {
		if serviceFailureSite(forged) != "unknown" {
			t.Fatal("service site admitted unreviewed input")
		}
		var out bytes.Buffer
		WriteServiceHostFailure(&out, &serviceFailure{cause: errors.New("private path"), site: forged}, forged)
		if out.String() != "native service host site=unknown\n" {
			t.Fatal("service site exposed unreviewed input")
		}
	}
}

func TestServiceFailureSiteJoinedShutdown(t *testing.T) {
	cause := errors.New("private Windows filesystem object required")
	primary := serviceFailureAt(cause, "supervisor_traversal")
	repair := serviceFailureAt(cause, "shutdown_repair")
	stop := serviceFailureAt(cause, "shutdown_stop")
	var out bytes.Buffer
	WriteServiceHostFailure(&out, errors.Join(primary, repair, stop, stop), "unknown")
	if out.String() != "native service host site=supervisor_traversal\nnative service host site=shutdown_repair\nnative service host site=shutdown_stop\n" {
		t.Fatal("joined host failures lost distinct bounded site receipts")
	}
	out.Reset()
	WriteServiceHostFailure(&out, nil, "host_config")
	if out.Len() != 0 {
		t.Fatal("successful host emitted a failure site")
	}
	if serviceFailureFallback(primary, "control_transport") != primary {
		t.Fatal("outer host boundary erased a decisive inner site")
	}
}

// Pin each production attachment, including all return expressions and defer
// assignments. A wrapper present elsewhere in a function cannot satisfy this
// contract. CRLF normalization precedes all multiline comparisons.
func TestServiceFailureSiteSourceContract(t *testing.T) {
	contracts := map[string][]string{
		"service_health.go": {
			`func (c *Cell) ServiceHealth(ctx context.Context) (result error) {
	ctx, cancel := c.deadline(ctx)
	defer cancel()
	site := serviceHealthStep(ctx, "health_lock")
	defer func() { result = serviceFailureAt(result, site) }()
	lock, err := c.own(false)`,
			`site = serviceHealthStep(ctx, "health_tree")
	d, err := c.ServiceDescriptor()`,
			`site = serviceHealthStep(ctx, "health_database")
	dsn := url.URL`,
			`site = serviceHealthStep(ctx, "health_credential")
	credential, err := c.serviceAPICredential(ctx, st, m)`,
			`site = serviceHealthStep(ctx, "health_probe")
	client := localHTTPClient()`,
			`site = serviceHealthStep(ctx, "health_ready")
	if !ready(ctx, client, m.Ports.ServerHealth) || !ready(ctx, client, m.Ports.WorkerHealth)`,
			`site = serviceHealthStep(ctx, "health_tree")
	return c.serviceLiveTree()`,
			`func serviceHealthStep(ctx context.Context, site string) string {
	site = serviceHealthSite(site)
	if progress, ok := ctx.Value(serviceHealthProgressKey{}).(*serviceHealthProgress); ok {
		progress.site = site
	}
	return site
}`,
			`func serviceHealthSite(site string) string {
	switch site {
	case "health_lock", "health_tree", "health_database", "health_credential", "health_probe", "health_ready":
		return site
	default:
		return "unknown"
	}
}`,
		},
		"service_foreground.go": {
			`if err := privateDir(c.path("service"), false); err != nil {
		return nil, serviceFailureAt(err, "lease_directory")`,
			`if err := servicePrivateRecord(c.path(".service-host.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, serviceFailureAt(err, "lease_record")`,
			`if err := writeAtomic(c.path("service", "lease.json"), record, nil); err != nil {
		_ = lock.Close()
		return nil, serviceFailureAt(err, "lease_write")`,
		},
		"service_supervisor.go": {
			`d, err := c.ServiceDescriptor()
	if err != nil {
		return serviceFailureAt(err, "initial_descriptor")`,
			`closeControl, err := c.serveServiceControl(commands)
	if err != nil {
		return serviceFailureFallback(err, "control_transport")`,
			`result = errors.Join(result, serviceFailureAt(repairErr, "shutdown_repair"), serviceFailureAt(stopErr, "shutdown_stop"))`,
			`return s.call(ctx, "reconcile", func(context.Context) error { return s.hooks.reconcile() })
	}); err != nil {
		return serviceFailureAt(err, "startup_reconcile")`,
			`return s.call(ctx, "repair", s.hooks.repair) }); err != nil {
		return serviceFailureAt(err, "startup_repair")`,
			`return s.call(ctx, "stop", s.hooks.stop) }); err != nil {
		return serviceFailureAt(err, "startup_stop")`,
			`func (s *supervisor) load(d ServiceDescriptor) (result error) {
	defer func() { result = serviceFailureFallback(result, "supervisor_load") }()`,
			`return serviceFailureAt(writeAtomic(s.cell.path("service", "supervisor.json"), s.record, nil), "supervisor_save")`,
			`func (s *supervisor) call(ctx context.Context, operation string, hook func(context.Context) error) (result error) {
	// No operation receipt may claim a hook which never ran.
	if err := ctx.Err(); err != nil {
		if cancellation := serviceControlCancellation(ctx); cancellation != nil {
			return cancellation
		}
		return err
	}
	defer func() {
		result = serviceFailureOperation(result, operation)
		// Some failed operations enter backoff instead of exiting the host.
		// Preserve their fixed site before that error is reduced to status.
		WriteServiceHostFailure(os.Stderr, result, "unknown")
	}()`,
			`if operation == "health" {
		s.healthProgress.site = "health_lock"
		var cancel context.CancelFunc
		hookCtx, cancel = context.WithTimeout(ctx, s.cell.cfg.Timeout)
		defer cancel()
		hookCtx = withServiceHealthProgress(hookCtx, &s.healthProgress)
	}`,
			`if ctx.Err() != nil {
		if operation == "health" {
			return serviceFailureFallback(ctx.Err(), serviceHealthSite(s.healthProgress.site))
		}
		return ctx.Err()
	}
	if hookCtx.Err() != nil {
		err = serviceFailureFallback(hookCtx.Err(), serviceHealthSite(s.healthProgress.site))
	}`,
			`if s.operation == "health" {
				fmt.Fprintf(os.Stderr, "service_shutdown phase=%s operation=health site=%s journal_pending=%t journal_read_ok=%t\n",
					s.phase, serviceHealthSite(s.healthProgress.site), progress.pending != nil || progress.tornTail, journalErr == nil)
			} else {
				fmt.Fprintf(os.Stderr, "service_shutdown phase=%s operation=%s journal_pending=%t journal_read_ok=%t\n",
					s.phase, s.operation, progress.pending != nil || progress.tornTail, journalErr == nil)
			}`,
			`d, err := s.cell.ServiceDescriptor()
	if cancellation := serviceControlCancellation(ctx); cancellation != nil {
		return cancellation
	}
	if err != nil {
		return serviceFailureAt(err, "supervisor_traversal")`,
			`return s.status, serviceFailureFallback(err, "supervisor_command")`,
			`status, err := s.command(commandCtx, command.action)
			cancel()
			WriteServiceHostFailure(os.Stderr, err, "supervisor_command")
			command.reply <- serviceReply{status: status, err: err}`,
		},
		"service_control.go": {`func (c *Cell) readServiceControlSecret() (secretBytes []byte, result error) {
	defer func() { result = serviceFailureAt(result, "control_secret") }()`,
			`if errors.As(err, &refusal) {
		var located *serviceFailure
		if errors.As(err, &located) {
			return serviceFailureAt(refusal, located.site)
		}`,
			`WriteServiceHostFailure(os.Stderr, c.removeServiceControlEndpoint(), "endpoint_remove")`,
		},
		"service_control_endpoint.go": {
			`func (c *Cell) readServiceControlEndpointRecord() (endpoint serviceControlEndpointRecord, result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_read") }()`,
			`func (c *Cell) publishServiceControlEndpointWithBoot(pipeName string, readBoot func() (string, error)) (endpoint serviceControlEndpointRecord, result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_write") }()`,
			`func (c *Cell) removeOwnedServiceControlEndpoint(owned serviceControlEndpointRecord) (result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_remove") }()`,
			`record, err := c.readServiceControlEndpointRecord()
	if err != nil {
		return serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_read")
	}`,
			`old, err := c.readServiceControlEndpointRecord()
	if err != nil {
		return serviceFailureAt(serviceEndpointRefused("host_identity"), "endpoint_read")
	}`,
			`func (c *Cell) refreshOwnedServiceControlEndpoint(d ServiceDescriptor) (result error) {
	defer func() { result = serviceFailureFallback(result, "endpoint_write") }()`,
		},
		"service_control_windows.go": {
			`d, err := c.ServiceDescriptor()
	if err != nil {
		return nil, serviceFailureAt(windowsControlRefusal("endpoint:record"), "endpoint_descriptor")`,
		},
		"service_native_diagnostics_test.go": {
			`var sites strings.Builder
	localcell.WriteServiceHostFailure(&sites, d.controlErr, "control_transport")
	for _, line := range strings.Split(strings.TrimSpace(sites.String()), "\n") {
		if line != "" {
			d.logf("%s", line)
		}
	}`,
		},
		"service_boot.go": {`func (c *Cell) bindServiceProcessBootWithReader(component string, readBoot func() (string, error)) (result error) {
	defer func() { result = serviceFailureFallback(result, "boot_binding") }()`},
		"service_descriptor.go": {`func (c *Cell) writeServiceDescriptor(d ServiceDescriptor) (result error) {
	defer func() { result = serviceFailureFallback(result, "descriptor_write") }()`},
		"../tools/localcellharness/service.go": {
			`defer func() {
		if action != "install" && action != "remove" {
			localcell.WriteServiceHostFailure(os.Stderr, result, site)
		}
	}()`,
			`site := "host_config"`, `site = "host_descriptor"`, `site = "host_prepare"`, `site = "host_persisted_config"`,
			`if action == "run" {
		site = "control_transport"
		return cell.Run(ctx, localcell.SupervisorPolicy{})
	}`,
			`site = "control_transport"
	adapter := localcell.NewServiceAdapter()`,
		},
	}
	var attached []string
	for path, snippets := range contracts {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read service site emitter contract")
		}
		source := strings.ReplaceAll(string(data), "\r\n", "\n")
		for _, snippet := range snippets {
			if strings.Count(source, snippet) != 1 {
				t.Fatal("service emitter lost its decisive site binding")
			}
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatal("cannot parse service site emitter contract")
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || (ident.Name != "serviceFailureAt" && ident.Name != "serviceFailureFallback") {
				return true
			}
			if len(call.Args) != 2 {
				t.Fatal("service emitter site has dynamic arguments")
			}
			literal, ok := call.Args[1].(*ast.BasicLit)
			if !ok && path == "service_supervisor.go" {
				// Only the health-specific sanitizer may supply dynamic substeps;
				// both exact call sites are pinned above.
				if sanitized, yes := call.Args[1].(*ast.CallExpr); yes && len(sanitized.Args) == 1 {
					if name, yes := sanitized.Fun.(*ast.Ident); yes && name.Name == "serviceHealthSite" {
						if site, yes := sanitized.Args[0].(*ast.SelectorExpr); yes && site.Sel.Name == "site" {
							if progress, yes := site.X.(*ast.SelectorExpr); yes && progress.Sel.Name == "healthProgress" {
								if receiver, yes := progress.X.(*ast.Ident); yes && receiver.Name == "s" {
									return true
								}
							}
						}
					}
				}
			}
			if !ok && path == "service_health.go" {
				// The exact deferred attachment and every closed step assignment
				// are pinned above; no other dynamic health site is admitted.
				if id, yes := call.Args[1].(*ast.Ident); yes && id.Name == "site" {
					return true
				}
			}
			if !ok && path == "service_control.go" {
				if sel, yes := call.Args[1].(*ast.SelectorExpr); yes && sel.Sel.Name == "site" {
					if id, yes := sel.X.(*ast.Ident); yes && id.Name == "located" {
						return true
					}
				}
			}
			if !ok {
				t.Fatal("service emitter site has dynamic arguments")
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil || serviceFailureSite(value) == "unknown" {
				t.Fatal("service emitter site is outside the reviewed vocabulary")
			}
			attached = append(attached, value)
			return true
		})
	}
	want := []string{"lease_directory", "lease_record", "lease_write", "initial_descriptor", "control_transport", "shutdown_repair", "shutdown_stop", "startup_reconcile", "startup_repair", "startup_stop", "supervisor_load", "supervisor_save", "supervisor_traversal", "supervisor_command", "control_secret", "control_cancelled", "endpoint_read", "endpoint_write", "endpoint_write", "endpoint_remove", "endpoint_descriptor", "endpoint_descriptor", "endpoint_lease", "endpoint_lease", "endpoint_read", "endpoint_read", "endpoint_descriptor", "boot_binding", "descriptor_write"}
	sort.Strings(attached)
	sort.Strings(want)
	if !reflect.DeepEqual(attached, want) {
		t.Fatal("production service emitter site inventory changed")
	}
}

// Count producers independently from the attachment inventory above. Adding a
// new raw filesystem/lifecycle return cannot pass merely because its function
// still contains the old reviewed wrapper somewhere else.
func TestServiceFailureSiteProducerInventory(t *testing.T) {
	producers := strings.Fields(`privateDir servicePrivateRecord openPrivateFile readPrivateFile readJSON readManifest writeAtomic writeAtomicBytes removePrivateFile validateRoot validatePrivate validatePrivateTree validateRootPath openLockFile serviceControlSecret ServiceDescriptor PrepareService New readServiceControlSecret readServiceControlEndpointRecord readServiceControlEndpointWithBoot readServiceControlEndpoint publishServiceControlEndpoint publishServiceControlEndpointWithBoot removeOwnedServiceControlEndpoint refreshOwnedServiceControlEndpoint removeServiceControlEndpoint listenServiceControl dialServiceControl serveServiceControl Control Run load save tick step call command Repair Stop Start SetServiceDesiredState RepairService setServiceFaulted bindServiceProcessBoot bindServiceProcessBootWithReader reconcileServiceBoot reconcileServiceBootWithReader reconcileOldBoot writeServiceDescriptor own newSupervisor acquireLease acquireServiceLease acquireServiceLeaseWithBoot stopServiceChildren ServiceHealth hook operation retryServiceContention`)
	selected := make(map[string]bool)
	for _, producer := range producers {
		selected[producer] = true
	}
	want := map[string]map[string]string{
		"service_health.go": {
			"ServiceHealth":        "ServiceDescriptor own readManifest",
			"serviceAPICredential": "readJSON servicePrivateRecord writeAtomic",
		},
		"service_foreground.go": {
			"acquireServiceLease":         "acquireServiceLeaseWithBoot",
			"acquireServiceLeaseWithBoot": "openLockFile privateDir servicePrivateRecord writeAtomic",
		},
		"service_supervisor.go": {
			"Run":                    "ServiceDescriptor acquireLease acquireServiceLeaseWithBoot call call call command load newSupervisor retryServiceContention retryServiceContention retryServiceContention retryServiceContention retryServiceContention serveServiceControl tick tick",
			"call":                   "hook",
			"command":                "RepairService ServiceDescriptor SetServiceDesiredState SetServiceDesiredState call step step step step",
			"failed":                 "save",
			"fault":                  "setServiceFaulted",
			"load":                   "readJSON servicePrivateRecord",
			"rememberProcesses":      "readJSON readJSON",
			"retryServiceContention": "operation",
			"save":                   "writeAtomic",
			"step":                   "ServiceDescriptor call call call call call call call save save save save",
			"tick":                   "step",
		},
		"service_control.go": {
			"Control":                  "dialServiceControl readServiceControlSecret",
			"readServiceControlSecret": "serviceControlSecret validateRoot",
			"serveServiceControl":      "listenServiceControl readServiceControlSecret removeServiceControlEndpoint",
		},
		"service_control_endpoint.go": {
			"publishServiceControlEndpoint":         "publishServiceControlEndpointWithBoot",
			"publishServiceControlEndpointWithBoot": "ServiceDescriptor readJSON servicePrivateRecord writeAtomic",
			"readServiceControlEndpoint":            "readServiceControlEndpointWithBoot",
			"readServiceControlEndpointRecord":      "openPrivateFile privateDir servicePrivateRecord",
			"readServiceControlEndpointWithBoot":    "ServiceDescriptor readJSON readServiceControlEndpointRecord servicePrivateRecord",
			"refreshOwnedServiceControlEndpoint":    "readServiceControlEndpointRecord writeAtomic",
			"removeOwnedServiceControlEndpoint":     "readServiceControlEndpointRecord removePrivateFile",
		},
		"service_control_windows.go": {
			"dialServiceControl":             "readServiceControlEndpoint",
			"listenServiceControl":           "ServiceDescriptor publishServiceControlEndpoint",
			"removeServiceControlEndpoint":   "removeOwnedServiceControlEndpoint",
			"validateServiceControlEndpoint": "readServiceControlEndpoint",
		},
		"service_boot.go": {
			"bindServiceProcessBoot":           "bindServiceProcessBootWithReader",
			"bindServiceProcessBootWithReader": "ServiceDescriptor readJSON writeAtomic",
			"reconcileOldBoot":                 "readJSON removePrivateFile removePrivateFile",
			"reconcileServiceBoot":             "ServiceDescriptor own readJSON reconcileOldBoot writeAtomic",
			"startServicePostgres":             "bindServiceProcessBoot",
		},
		"service_descriptor.go": {
			"PrepareService":               "ServiceDescriptor own privateDir readManifest serviceControlSecret serviceControlSecret writeAtomicBytes writeServiceDescriptor",
			"RepairService":                "Repair ServiceDescriptor SetServiceDesiredState own stopServiceChildren writeServiceDescriptor",
			"ServiceDescriptor":            "privateDir readJSON readManifest servicePrivateRecord validateRoot",
			"SetServiceDesiredState":       "own",
			"serviceControlSecret":         "privateDir readPrivateFile servicePrivateRecord",
			"serviceStartFence":            "ServiceDescriptor",
			"setServiceDesiredStateLocked": "ServiceDescriptor writeServiceDescriptor",
			"setServiceFaulted":            "ServiceDescriptor own writeServiceDescriptor",
			"writeServiceDescriptor":       "refreshOwnedServiceControlEndpoint removePrivateFile writeAtomic writeAtomic",
		},
		"../tools/localcellharness/service.go": {
			"runServiceCommandForGOOS": "Control New New PrepareService RepairService Run ServiceDescriptor ServiceDescriptor SetServiceDesiredState Stop",
		},
	}
	for path, expected := range want {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read service producer inventory")
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, strings.ReplaceAll(string(source), "\r\n", "\n"), 0)
		if err != nil {
			t.Fatal("cannot parse service producer inventory")
		}
		got := make(map[string]string)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			var calls []string
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch callee := call.Fun.(type) {
				case *ast.Ident:
					name = callee.Name
				case *ast.SelectorExpr:
					name = callee.Sel.Name
					if receiver, ok := callee.X.(*ast.Ident); ok {
						// These same-spelled standard-library methods cannot emit
						// the private-filesystem error under review.
						if name == "New" && receiver.Name != "localcell" || name == "Stop" && (receiver.Name == "timer" || receiver.Name == "ticker") || name == "Control" && receiver.Name == "sd" {
							return true
						}
					}
				}
				if selected[name] {
					calls = append(calls, name)
				}
				return true
			})
			if len(calls) != 0 {
				sort.Strings(calls)
				got[fn.Name.Name] = strings.Join(calls, " ")
			}
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("service privacy producer inventory changed in %s", path)
		}
	}
}

func TestServiceFailureSiteEndpointReadPropagation(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, descriptor := descriptorTestCell(t)
	record, _, _ := endpointFixture(t)
	c.serviceControlOwned = &record
	for _, tc := range []struct {
		name string
		run  func() error
		text string
	}{
		{"read", func() error { _, err := c.readServiceControlEndpointRecord(); return err }, "localcell: service_control_refused: endpoint:record"},
		{"refresh", func() error { return c.refreshOwnedServiceControlEndpoint(descriptor) }, "localcell: service_control_refused: endpoint:host_identity"},
		{"remove", func() error { return c.removeOwnedServiceControlEndpoint(record) }, "localcell: service_control_refused: endpoint:host_identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if !errors.Is(err, ErrServiceControlRefused) || err.Error() != tc.text {
				t.Fatal("endpoint read failure changed its fixed refusal or identity")
			}
			var receipt bytes.Buffer
			WriteServiceHostFailure(&receipt, err, "control_transport")
			if receipt.String() != "native service host site=endpoint_read\n" {
				t.Fatal("endpoint read failure lost its site at a service boundary")
			}
		})
	}
}

func TestServiceFailureSiteTransportPropagation(t *testing.T) {
	refusal := serviceEndpointRefused("record")
	wrapped := &url.Error{Op: "Post", URL: "http://synthetic-private.invalid", Err: &serviceControlDialError{err: serviceFailureAt(refusal, "endpoint_read")}}
	classified := serviceControlTransportError(wrapped)
	if !errors.Is(classified, ErrServiceControlRefused) || classified.Error() != refusal.Error() {
		t.Fatal("transport site retention changed fixed refusal or exposed its URL")
	}
	var receipt bytes.Buffer
	WriteServiceHostFailure(&receipt, classified, "control_transport")
	if receipt.String() != "native service host site=endpoint_read\n" {
		t.Fatal("transport classification discarded its decisive endpoint site")
	}
}

func TestServiceFailureSiteOperationVocabulary(t *testing.T) {
	want := map[string]string{"start": "supervisor_start", "stop": "supervisor_stop", "repair": "supervisor_repair", "reconcile": "supervisor_reconcile", "health": "supervisor_health", "": "unknown"}
	source, err := os.ReadFile("service_failure_site.go")
	if err != nil {
		t.Fatal("cannot read service operation sites")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "sites.go", source, 0)
	if err != nil {
		t.Fatal("cannot parse service operation sites")
	}
	got := make(map[string]string)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "serviceFailureOperation" {
			continue
		}
		if len(fn.Body.List) != 1 {
			t.Fatal("service operation vocabulary has an unreviewed return path")
		}
		switchOperation, ok := fn.Body.List[0].(*ast.SwitchStmt)
		if !ok || switchOperation.Init != nil {
			t.Fatal("service operation vocabulary lost its closed switch")
		}
		input, ok := switchOperation.Tag.(*ast.Ident)
		if !ok || input.Name != "operation" {
			t.Fatal("service operation vocabulary switched on an unreviewed input")
		}
		ast.Inspect(fn, func(node ast.Node) bool {
			branch, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			if len(branch.List) > 1 || len(branch.Body) != 1 {
				t.Fatal("service operation site mapping changed")
			}
			key := ""
			if len(branch.List) == 1 {
				literal, ok := branch.List[0].(*ast.BasicLit)
				if !ok {
					t.Fatal("service operation admitted a dynamic name")
				}
				key, err = strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal("service operation admitted an invalid name")
				}
			}
			ret, ok := branch.Body[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				t.Fatal("service operation site lost its returned binding")
			}
			call, ok := ret.Results[0].(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				t.Fatal("service operation site lost its returned binding")
			}
			callee, ok := call.Fun.(*ast.Ident)
			if !ok || callee.Name != "serviceFailureFallback" {
				t.Fatal("service operation overwrote an existing precise site")
			}
			cause, ok := call.Args[0].(*ast.Ident)
			if !ok || cause.Name != "err" {
				t.Fatal("service operation changed its error cause")
			}
			literal, ok := call.Args[1].(*ast.BasicLit)
			if !ok {
				t.Fatal("service operation admitted a dynamic site")
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal("service operation admitted an invalid site")
			}
			if _, duplicate := got[key]; duplicate {
				t.Fatal("service operation duplicated a reviewed name")
			}
			got[key] = value
			return true
		})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("service operation site vocabulary changed")
	}
	cause := errors.New("private Windows filesystem object required")
	precise := serviceFailureAt(cause, "boot_binding")
	for operation, site := range want {
		var receipt bytes.Buffer
		wrapped := serviceFailureOperation(cause, operation)
		WriteServiceHostFailure(&receipt, wrapped, "unknown")
		if receipt.String() != "native service host site="+site+"\n" || !errors.Is(wrapped, cause) || wrapped.Error() != cause.Error() {
			t.Fatal("service operation lost its distinct fixed receipt or original error")
		}
		if serviceFailureOperation(precise, operation) != precise || serviceFailureOperation(nil, operation) != nil {
			t.Fatal("service operation replaced a precise site or successful result")
		}
	}
	var receipt bytes.Buffer
	WriteServiceHostFailure(&receipt, serviceFailureOperation(cause, "synthetic/private"), "unknown")
	if receipt.String() != "native service host site=unknown\n" {
		t.Fatal("unknown operation exposed unreviewed data")
	}
}
