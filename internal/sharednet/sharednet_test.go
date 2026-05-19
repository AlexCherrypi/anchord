package sharednet

import (
	"strings"
	"testing"
)

// F-44 acceptance — Spec §Acceptance "Unit" bullets 1-6 + the
// authentik-style multi-transit case.

func TestPick_BackendCount_PicksHighest(t *testing.T) {
	p, err := New([]string{"ix-authentik_transit_a", "ix-authentik_transit_f", "dmz_macvlan"}, "dmz_macvlan", "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	// Two backends on transit_f, one on transit_a — transit_f wins.
	choice := p.Pick(map[string]int{
		"ix-authentik_transit_a": 1,
		"ix-authentik_transit_f": 2,
	})
	if choice != "ix-authentik_transit_f" {
		t.Errorf("got %q, want ix-authentik_transit_f (highest backend count)", choice)
	}
	if !p.Settled() {
		t.Error("picker should settle once a backend is observed on chosen")
	}
}

// F-44: with the bug-triggering input from the Authentik migration
// (three transit networks, backends visible only on the "_f" one),
// the picker must NOT pick the docker-proxy transit.
func TestPick_AuthentikFrigateBugFixed(t *testing.T) {
	p, err := New([]string{
		"ix-authentik_transit_dp",
		"ix-authentik_transit_f",
		"ix-authentik_backend",
		"dmz_macvlan",
	}, "dmz_macvlan", "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	choice := p.Pick(map[string]int{
		"ix-authentik_transit_f": 1, // fe-anchor-frigate lives here
	})
	if choice != "ix-authentik_transit_f" {
		t.Errorf("regression: picker chose %q instead of ix-authentik_transit_f", choice)
	}
}

// F-44 tie behaviour: equal backend counts → prefer "transit" name;
// then alphabetical.
func TestPick_TieTransitPreferred(t *testing.T) {
	p, _ := New([]string{"backend", "transit_a"}, "", "")
	choice := p.Pick(map[string]int{
		"backend":   1,
		"transit_a": 1,
	})
	if choice != "transit_a" {
		t.Errorf("tie + transit-preference: got %q want transit_a", choice)
	}
}

func TestPick_TieAllTransitAlphabetical(t *testing.T) {
	p, _ := New([]string{"transit_z", "transit_a", "transit_m"}, "", "")
	choice := p.Pick(map[string]int{
		"transit_a": 1, "transit_m": 1, "transit_z": 1,
	})
	if choice != "transit_a" {
		t.Errorf("all-tie all-transit: got %q want transit_a", choice)
	}
}

// F-44: empty backend set → fall back to F-38 ordering (transit
// preference, then alphabetic); do NOT settle.
func TestPick_EmptyBackendSet_FallbackNoSettle(t *testing.T) {
	p, _ := New([]string{"backend", "transit_a", "transit_z"}, "", "")
	choice := p.Pick(map[string]int{})
	if choice != "transit_a" {
		t.Errorf("fallback should pick alpha-first transit, got %q", choice)
	}
	if p.Settled() {
		t.Error("zero-backend fallback must NOT settle (re-eval on later reconciles)")
	}
}

// F-44: fall-back, then on a later reconcile a backend appears on
// the OTHER transit network — picker switches and settles. This is
// the F-44 motivating bug fix path.
func TestPick_FallbackThenSwitchOnFirstBackend(t *testing.T) {
	p, _ := New([]string{"transit_a", "transit_b"}, "", "")
	first := p.Pick(map[string]int{}) // empty: falls back to transit_a (alpha)
	if first != "transit_a" || p.Settled() {
		t.Fatalf("setup: got %q settled=%v", first, p.Settled())
	}
	// Backend later appears on transit_b.
	second := p.Pick(map[string]int{"transit_b": 1})
	if second != "transit_b" {
		t.Errorf("expected switch to transit_b once backend appears, got %q", second)
	}
	if !p.Settled() {
		t.Error("picker should settle once a real backend has been observed")
	}
}

// F-44 stability: once settled, do NOT switch even if backends
// later appear on another candidate. First-wins.
func TestPick_StableOnceSettled(t *testing.T) {
	p, _ := New([]string{"transit_a", "transit_b"}, "", "")
	first := p.Pick(map[string]int{"transit_a": 1})
	if first != "transit_a" || !p.Settled() {
		t.Fatalf("setup: got %q settled=%v", first, p.Settled())
	}
	// Now transit_b has more backends — must NOT switch.
	second := p.Pick(map[string]int{"transit_a": 1, "transit_b": 99})
	if second != "transit_a" {
		t.Errorf("must not switch once settled, got %q", second)
	}
}

// F-44 ANCHORD_SHARED_NETWORK override: pinned value wins regardless
// of backend distribution. Settles immediately.
func TestPick_PinnedOverride(t *testing.T) {
	p, err := New([]string{"transit_a", "transit_b", "dmz"}, "dmz", "transit_b")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	choice := p.Pick(map[string]int{"transit_a": 99}) // would otherwise win
	if choice != "transit_b" {
		t.Errorf("pinned override ignored: got %q", choice)
	}
	if !p.Settled() {
		t.Error("pinned mode should settle immediately")
	}
}

// F-44 acceptance: ANCHORD_SHARED_NETWORK set to a name NOT in
// self-networks is a fatal configuration error.
func TestNew_PinnedNotInSelfNetworks_Rejected(t *testing.T) {
	_, err := New([]string{"transit_a", "dmz"}, "dmz", "no-such-net")
	if err == nil {
		t.Fatal("expected error for pinned name not in self-networks")
	}
	if !strings.Contains(err.Error(), "ANCHORD_SHARED_NETWORK") {
		t.Errorf("error must mention the env var name, got: %v", err)
	}
}

// EXT_NETWORK is excluded from candidates. If EXT is the only
// non-pinned option, candidates is empty and Pick returns "".
// (The caller — main.go — should already have failed earlier in
// detect-shared-network, but this guards the picker contract.)
func TestPick_AllExcluded_ReturnsEmpty(t *testing.T) {
	p, err := New([]string{"only-ext"}, "only-ext", "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	choice := p.Pick(map[string]int{})
	if choice != "" {
		t.Errorf("got %q, want empty when no candidates", choice)
	}
}

func TestNew_CandidatesSortedAlpha(t *testing.T) {
	p, err := New([]string{"z", "a", "m", "dmz"}, "dmz", "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	got := p.Candidates()
	want := []string{"a", "m", "z"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("got %v want %v", got, want)
	}
}

// F-44: CountBackendsPerNetwork sums attachments across backend
// containers; a backend on N networks contributes N counts.
func TestCountBackendsPerNetwork(t *testing.T) {
	got := CountBackendsPerNetwork([][]string{
		{"transit_a", "backend"},
		{"transit_a"},
		{"transit_b", "backend"},
	})
	if got["transit_a"] != 2 {
		t.Errorf("transit_a: got %d want 2", got["transit_a"])
	}
	if got["transit_b"] != 1 {
		t.Errorf("transit_b: got %d want 1", got["transit_b"])
	}
	if got["backend"] != 2 {
		t.Errorf("backend: got %d want 2", got["backend"])
	}
	if got["never-seen"] != 0 {
		t.Errorf("never-seen should be 0 (zero-value), got %d", got["never-seen"])
	}
}

// F-44 case insensitivity: "Transit" / "TRANSIT" / "FooTransitBar"
// should all be recognised by the transit-preference heuristic.
func TestPick_TieTransitCaseInsensitive(t *testing.T) {
	for _, name := range []string{"Transit", "TRANSIT", "FooTransitBar"} {
		t.Run(name, func(t *testing.T) {
			p, _ := New([]string{"backend", name}, "", "")
			choice := p.Pick(map[string]int{"backend": 1, name: 1})
			if choice != name {
				t.Errorf("case-insensitive transit match failed for %q, got %q", name, choice)
			}
		})
	}
}
