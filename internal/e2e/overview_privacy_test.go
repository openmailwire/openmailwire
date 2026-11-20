package e2e

import (
	"testing"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// Workers are shared by every project. On a multi-tenant install a tenant's
// overview shows their health but not the operator's machine names,
// addresses or versions; the operator's project (OMW_OPERATOR_PROJECTS), or
// the only project of a single-tenant install, sees them.
func TestOverviewWorkerPrivacy(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.st.Heartbeat(e.ctx, e.workerID, protocol.HeartbeatRequest{PublicIP: "203.0.113.7", EHLO: "w1.openmailwire.test", Version: "v9.9.9"}); err != nil {
		t.Fatal(err)
	}
	firstWorker := func(h *harness) map[string]any {
		t.Helper()
		code, v := h.call("GET", "/overview", "", nil)
		if code != 200 {
			t.Fatalf("overview: %d %v", code, v)
		}
		ws, _ := v["workers"].([]any)
		if len(ws) != 1 {
			t.Fatalf("workers: %v", v["workers"])
		}
		return ws[0].(map[string]any)
	}

	// Single-tenant install: the only project sees the details.
	if w := firstWorker(e.harness); w["name"] != "w1" || w["ip"] != "203.0.113.7" || w["version"] != "v9.9.9" {
		t.Fatalf("single project sees: %v", w)
	}

	// A second project makes it multi-tenant: details are hidden from both
	// until an operator project is named.
	other := e.otherProject()
	for _, h := range []*harness{e.harness, other} {
		w := firstWorker(h)
		if w["name"] != "worker 1" || w["ip"] != "" || w["version"] != "" {
			t.Fatalf("tenant sees worker details: %v", w)
		}
		if _, ok := w["online"]; !ok {
			t.Fatalf("tenant lost the worker's health: %v", w)
		}
	}

	e.srv.Cfg.OperatorProjects = []string{e.projectID}
	if w := firstWorker(e.harness); w["name"] != "w1" || w["ip"] != "203.0.113.7" {
		t.Fatalf("operator project sees: %v", w)
	}
	if w := firstWorker(other); w["name"] != "worker 1" || w["ip"] != "" {
		t.Fatalf("non-operator project sees: %v", w)
	}
}
