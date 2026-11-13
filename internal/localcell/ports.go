package localcell

import "net"

// An empty component selects the complete initial allocation. Start selects
// exactly one component, preserving even the saved ports of idle peers.
func (p *Ports) selected(component string) []*int {
	switch component {
	case "postgres":
		return []*int{&p.Postgres}
	case "server":
		return []*int{&p.ServerAPI, &p.ServerHealth, &p.ServerMetrics}
	case "worker":
		return []*int{&p.WorkerHealth, &p.WorkerMetrics}
	default:
		return []*int{&p.Postgres, &p.ServerAPI, &p.ServerHealth, &p.ServerMetrics, &p.WorkerHealth, &p.WorkerMetrics}
	}
}

func allocatePorts(previous Ports, component string) (Ports, error) {
	ports := previous
	listeners := make([]net.Listener, 0, 12)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	avoid := make(map[int]bool)
	for _, port := range previous.all() {
		avoid[port] = true
	}
	for _, port := range ports.selected(component) {
		for {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return Ports{}, fixedCause("cannot allocate local cell loopback ports")
			}
			// Retain rejected candidates too. At most the six old ports can be
			// rejected, and a held listener cannot be allocated a second time.
			listeners = append(listeners, listener)
			candidate := listener.Addr().(*net.TCPAddr).Port
			if !avoid[candidate] {
				*port = candidate
				break
			}
		}
	}
	return ports, nil
}

// Probe without retaining reservations: the eventual bind remains authoritative.
func probePorts(ports Ports, component string) error {
	for _, port := range ports.selected(component) {
		listener, err := net.Listen("tcp4", addr(*port))
		if err != nil {
			return fixedCause("saved local cell port is occupied")
		}
		_ = listener.Close()
	}
	return nil
}

// Install and per-component Start recovery share the same bounded allocation
// and probe. Losing a just-released allocation consumes an attempt.
func (c *Cell) allocateAvailablePorts(previous Ports, component string) (Ports, error) {
	for range 3 {
		ports, err := allocatePorts(previous, component)
		if err != nil {
			return Ports{}, err
		}
		if c.afterPortAllocation != nil {
			c.afterPortAllocation(ports)
		}
		if probePorts(ports, component) == nil {
			return ports, nil
		}
	}
	return Ports{}, fixedCause("cannot allocate local cell loopback ports")
}

func (c *Cell) prepareComponentPorts(m *Manifest, component string, running, force bool) error {
	return c.prepareComponentPortsFor("start", m, component, running, force)
}

// prepareComponentPortsFor journals a reallocation under the calling operation.
func (c *Cell) prepareComponentPortsFor(operation string, m *Manifest, component string, running, force bool) error {
	if running {
		return nil
	}
	if !force && probePorts(m.Ports, component) == nil {
		// A prior interrupted reallocation may have published the manifest
		// before replacing PostgreSQL's config. Reconcile every stopped
		// launch from the durable manifest, even when no new ports are needed.
		if component == "postgres" {
			return c.configurePostgres(m)
		}
		return nil
	}
	ports, err := c.allocateAvailablePorts(m.Ports, component)
	if err != nil {
		return err
	}
	candidate := *m
	candidate.Ports = ports
	if err := writeAtomic(c.path("manifest.json"), &candidate, nil); err != nil {
		return err
	}
	*m = candidate
	// PostgreSQL reads its port from its configuration, unlike the child
	// environment used by server and worker. Only a stopped cluster gets here.
	if component == "postgres" {
		if err := c.configurePostgres(m); err != nil {
			return err
		}
	}
	return c.journalCause(operation, component, "started", "port_reallocated="+component)
}
