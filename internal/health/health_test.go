package health

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLiveness_AlwaysOK(t *testing.T) {
	// Liveness must be 200 regardless of any tracker state — it is the
	// "is the process serving HTTP?" signal, not a data-plane probe
	// (SPEC F-33). A fresh tracker (everything false) must still pass.
	t.Run("fresh tracker", func(t *testing.T) {
		assertGet(t, LivenessHandler(), http.StatusOK, "ok\n")
	})
	t.Run("tracker with state", func(t *testing.T) {
		// Liveness handler doesn't even take a tracker — proves by
		// construction that liveness can't be flipped by data-plane
		// state. Just sanity-check it again here.
		assertGet(t, LivenessHandler(), http.StatusOK, "ok\n")
	})
}

func TestNetworkAnchorReadiness_StateMachine(t *testing.T) {
	tr := NewTracker()
	h := NetworkAnchorReadinessHandler(tr)

	// Cold: neither bit flipped → 503 with both conditions unmet.
	assertStatus(t, h, http.StatusServiceUnavailable)
	assertBodyContains(t, h, "tables_installed: false", "reconciled: false")

	// Tables only → still 503 (need both per F-34).
	tr.MarkTablesInstalled()
	assertStatus(t, h, http.StatusServiceUnavailable)
	assertBodyContains(t, h, "tables_installed: true", "reconciled: false")

	// Both → 200.
	tr.MarkReconciled()
	assertGet(t, h, http.StatusOK, "ready\n")
}

func TestNetworkAnchorReadiness_ReconcileAloneNotReady(t *testing.T) {
	// Edge case: a reconcile signal arrives before tables_installed
	// (shouldn't happen in normal startup, but the handler must not
	// rely on that ordering).
	tr := NewTracker()
	tr.MarkReconciled()
	h := NetworkAnchorReadinessHandler(tr)
	assertStatus(t, h, http.StatusServiceUnavailable)
	assertBodyContains(t, h, "tables_installed: false", "reconciled: true")
}

func TestServiceAnchorReadiness_StateMachine(t *testing.T) {
	tr := NewTracker()
	h := ServiceAnchorReadinessHandler(tr)

	assertStatus(t, h, http.StatusServiceUnavailable)
	assertBodyContains(t, h, "route_installed: false")

	tr.MarkRouteInstalled()
	assertGet(t, h, http.StatusOK, "ready\n")
}

func TestMarks_AreIdempotent(t *testing.T) {
	// SPEC F-34/F-35 say marks are idempotent. Concurrent or repeated
	// signals from a busy reconciler must not corrupt the state.
	tr := NewTracker()
	for i := 0; i < 100; i++ {
		tr.MarkTablesInstalled()
		tr.MarkReconciled()
		tr.MarkRouteInstalled()
	}
	assertGet(t, NetworkAnchorReadinessHandler(tr), http.StatusOK, "ready\n")
	assertGet(t, ServiceAnchorReadinessHandler(tr), http.StatusOK, "ready\n")
}

// --- helpers ---

func assertGet(t *testing.T, h http.Handler, wantStatus int, wantBody string) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != wantStatus {
		t.Errorf("status=%d want=%d", rr.Code, wantStatus)
	}
	body, _ := io.ReadAll(rr.Body)
	if string(body) != wantBody {
		t.Errorf("body=%q want=%q", body, wantBody)
	}
}

func assertStatus(t *testing.T, h http.Handler, want int) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != want {
		t.Errorf("status=%d want=%d", rr.Code, want)
	}
}

func assertBodyContains(t *testing.T, h http.Handler, parts ...string) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	body, _ := io.ReadAll(rr.Body)
	for _, p := range parts {
		if !strings.Contains(string(body), p) {
			t.Errorf("body missing %q; got:\n%s", p, body)
		}
	}
}
