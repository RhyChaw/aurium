package app

import "testing"

// A reviewer flagged that without a concrete Executor, aurium_exec answers
// "not available" forever and host placement cannot run a single command
// (§10, host placement). This is the one place that wiring happens, so it is
// the one place a regression would be silent.
func TestOpenWiresAGatewayExecutor(t *testing.T) {
	a := newApp(t)
	if a.Gateway.Executor == nil {
		t.Fatal("Open must wire a concrete Executor onto the Gateway, or aurium_exec never works")
	}
	if a.Gateway.Delegator == nil {
		t.Fatal("Open must wire a concrete Delegator onto the Gateway")
	}
	if a.Manager.HostAuriumURL == "" {
		t.Fatal("Open must give the manager a host-reachable daemon address, " +
			"or a host-sandboxed turn's MCP config names an endpoint host.docker.internal " +
			"cannot resolve outside a container")
	}
}
