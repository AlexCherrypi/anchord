// Package health provides /healthz (liveness) and /readyz (readiness)
// HTTP handlers for both anchord modes.
//
// The Tracker is a tiny state object that callers update from inside
// their loops:
//
//   - network-anchor: NAT manager calls MarkTablesInstalled() after
//     Setup(); reconciler calls MarkReconciled() after the first
//     successful apply.
//   - service-anchor: Manager calls MarkRouteInstalled() after the
//     first successful applyRoute call.
//
// The handlers report 200 once the relevant predicates hold and 503
// otherwise — see SPEC §2.8 for the formal contract.
package health

import (
	"fmt"
	"io"
	"net/http"
	"sync"
)

// Tracker holds the readiness state for one anchord mode. Both modes
// share the type; only the predicates that matter for the active
// mode are flipped.
type Tracker struct {
	mu              sync.Mutex
	tablesInstalled bool // network-anchor only
	reconciled      bool // network-anchor only
	routeInstalled  bool // service-anchor only
}

// NewTracker returns a Tracker with all predicates false.
func NewTracker() *Tracker { return &Tracker{} }

// MarkTablesInstalled flips the network-anchor "nftables ready" bit.
func (t *Tracker) MarkTablesInstalled() {
	t.mu.Lock()
	t.tablesInstalled = true
	t.mu.Unlock()
}

// MarkReconciled flips the network-anchor "first reconcile done" bit.
// Idempotent — subsequent calls are no-ops.
func (t *Tracker) MarkReconciled() {
	t.mu.Lock()
	t.reconciled = true
	t.mu.Unlock()
}

// MarkRouteInstalled flips the service-anchor "default route installed" bit.
// Idempotent.
func (t *Tracker) MarkRouteInstalled() {
	t.mu.Lock()
	t.routeInstalled = true
	t.mu.Unlock()
}

// LivenessHandler always returns 200 ok. Liveness is "is the process
// running and serving HTTP" — orthogonal to data-plane state. A
// liveness probe that flipped on transient issues would cause restart
// storms, which is the wrong shape (SPEC F-33).
func LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
}

// NetworkAnchorReadinessHandler emits 200 once both nftables tables
// are installed AND a reconcile has completed; 503 with the unmet
// conditions otherwise (SPEC F-34).
func NetworkAnchorReadinessHandler(t *Tracker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.mu.Lock()
		tables := t.tablesInstalled
		recon := t.reconciled
		t.mu.Unlock()

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if tables && recon {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ready\n")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "tables_installed: %v\nreconciled: %v\n", tables, recon)
	})
}

// ServiceAnchorReadinessHandler emits 200 once at least one default
// route has been installed; 503 otherwise (SPEC F-35).
func ServiceAnchorReadinessHandler(t *Tracker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.mu.Lock()
		ok := t.routeInstalled
		t.mu.Unlock()

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if ok {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ready\n")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "route_installed: false\n")
	})
}
