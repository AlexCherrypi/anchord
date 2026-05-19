// Package sharednet decides which Docker network anchord uses to
// read backend IPs from — the "shared network" between the
// network-anchor and its backends.
//
// The naive approach (post-F-38) is "any non-EXT network anchord is
// on; prefer one named *transit*; else Go-map-random". That breaks
// the moment anchord is on multiple transit-named networks at once
// (real-world case: Authentik with one transit per identity plus a
// shared docker-socket-proxy transit). The wrong-network pick costs
// the operator a `"no usable IP for container <foo>"` log line and
// silent zero DNAT entries.
//
// F-44 replaces the heuristic with co-attachment counting:
//
//   1. Candidate set = self-networks minus ANCHORD_EXT_NETWORK.
//   2. If ANCHORD_SHARED_NETWORK is set, return that and skip
//      the heuristic. Fatal if it's not in self-networks.
//   3. Otherwise pick the candidate with the highest backend count.
//      Ties: prefer one whose name contains "transit"
//      (case-insensitive). Secondary tie: alphabetical first.
//   4. If no backends exist yet (compose-up still in flight), fall
//      back to the deterministic-by-alphabet "transit"-preference
//      pick and let subsequent reconciles re-evaluate.
//
// The Picker is stateful so the choice doesn't flap: once a backend
// has been observed on the currently-chosen network, that network is
// "settled" and Pick() will not switch even if a backend later
// appears on another candidate.
package sharednet

import (
	"fmt"
	"sort"
	"strings"
)

// Picker holds the resolution state for one network-anchor instance.
// Construct via New; call Pick on every reconcile (cheap).
type Picker struct {
	// candidates is the alphabetically-sorted list of non-EXT
	// networks anchord is attached to. Stable for the process'
	// lifetime.
	candidates []string

	// pinned is the operator-supplied ANCHORD_SHARED_NETWORK value;
	// empty means "use the heuristic". When non-empty, Pick always
	// returns it regardless of backend co-attachment.
	pinned string

	// chosen is the current pick. Empty before the first Pick call.
	chosen string

	// settled flips to true the first time Pick observes a backend
	// on `chosen`. Once true, the chosen network is locked in for
	// the rest of the process — no flapping when backends later
	// appear on other candidates.
	settled bool
}

// New constructs a Picker. selfNetworks is the list of Docker
// networks the anchord container is attached to (e.g. from
// ContainerInspect on self). extNetwork is the value of
// ANCHORD_EXT_NETWORK (excluded from the candidate set). pinned is
// the value of ANCHORD_SHARED_NETWORK ("" means heuristic mode).
//
// Returns an error iff `pinned` is set to a name not present in
// selfNetworks — that's a configuration typo and we want to fail
// fast, the same way ANCHORD_EXT_NETWORK resolution does.
func New(selfNetworks []string, extNetwork, pinned string) (*Picker, error) {
	candidates := make([]string, 0, len(selfNetworks))
	inSelf := map[string]struct{}{}
	for _, n := range selfNetworks {
		inSelf[n] = struct{}{}
		if n == extNetwork {
			continue
		}
		candidates = append(candidates, n)
	}
	sort.Strings(candidates)
	if pinned != "" {
		if _, ok := inSelf[pinned]; !ok {
			return nil, fmt.Errorf("ANCHORD_SHARED_NETWORK=%q not present on self (have %v)", pinned, selfNetworks)
		}
	}
	return &Picker{
		candidates: candidates,
		pinned:     pinned,
	}, nil
}

// Candidates returns the alphabetically-sorted list of non-EXT
// networks anchord is on. Useful for logging at startup.
func (p *Picker) Candidates() []string {
	out := make([]string, len(p.candidates))
	copy(out, p.candidates)
	return out
}

// Settled reports whether the picker has locked in a choice based on
// observed backend co-attachment. Once true, Pick is a no-op.
func (p *Picker) Settled() bool { return p.settled }

// Pick returns the network to use for backend IP reads. The
// `backendNetworks` argument maps each network name to the number of
// expected backends attached to it (computed by the caller from the
// most recent ContainerList result, before pickIPs runs).
//
// Semantics:
//   - Pinned mode (ANCHORD_SHARED_NETWORK set): always returns the
//     pinned name. Calling Pick is essentially free.
//   - Heuristic mode, first call with at least one positive
//     backendNetworks entry on a candidate: picks the network with
//     the highest count (ties: "transit" preference, then alpha),
//     marks settled, returns.
//   - Heuristic mode, first call with no positive entries (empty
//     map or all-zero counts on candidates): falls back to the F-38
//     ordering — "transit" preference, alphabetic — and returns
//     without settling. Subsequent calls may revisit.
//   - Heuristic mode, already settled: returns the stored choice
//     unchanged.
//
// Pick never returns an empty string when at least one candidate
// exists. Returns "" only when the candidate list is empty (e.g.
// anchord is on a single network and that network is EXT_NETWORK,
// which is itself a misconfiguration the caller should already have
// caught — defensive return rather than panic).
func (p *Picker) Pick(backendNetworks map[string]int) string {
	if p.pinned != "" {
		// Pinned mode: ignore backend counts entirely.
		p.chosen = p.pinned
		p.settled = true
		return p.pinned
	}
	if len(p.candidates) == 0 {
		return ""
	}
	if p.settled {
		// Once a backend has been observed on chosen, do not
		// re-evaluate. F-44 §"Stable once decided".
		return p.chosen
	}

	// Find the candidate with the highest backend count. Tie-break
	// by "transit" preference then alphabetic. Iterate
	// `p.candidates` (already sorted alpha) so the alpha-tie path
	// is implicit.
	bestCount := -1
	best := ""
	bestHasTransit := false
	for _, c := range p.candidates {
		count := backendNetworks[c]
		hasTransit := containsFold(c, "transit")
		switch {
		case count > bestCount:
			bestCount = count
			best = c
			bestHasTransit = hasTransit
		case count == bestCount:
			// Same count — apply tie-breakers.
			if hasTransit && !bestHasTransit {
				best = c
				bestHasTransit = true
			}
			// Else: alpha — we iterate sorted, so the
			// already-set `best` is alpha-earlier; keep it.
		}
	}

	if bestCount > 0 {
		// Real signal: at least one backend on `best`. Settle on it.
		p.chosen = best
		p.settled = true
		return best
	}

	// Zero backends visible anywhere. Fall back to the F-38-style
	// heuristic pick and DON'T settle, so subsequent reconciles
	// can revisit when a backend finally appears.
	for _, c := range p.candidates {
		if containsFold(c, "transit") {
			p.chosen = c
			return c
		}
	}
	p.chosen = p.candidates[0]
	return p.chosen
}

// Chosen returns the most recent pick (may be empty if Pick has
// never been called).
func (p *Picker) Chosen() string { return p.chosen }

// containsFold is a case-insensitive substring check.
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// CountBackendsPerNetwork builds the input map for Pick: for each
// backend container, count its attachments to each network. Caller
// supplies the per-container network list (extracted from
// container.Summary.NetworkSettings.Networks).
//
// Networks not in self-attachments contribute too — that's fine,
// Pick only looks up its own candidates.
func CountBackendsPerNetwork(backendNetworks [][]string) map[string]int {
	out := map[string]int{}
	for _, nets := range backendNetworks {
		for _, n := range nets {
			out[n]++
		}
	}
	return out
}
