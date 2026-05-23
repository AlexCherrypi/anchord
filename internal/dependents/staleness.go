// Package dependents detects containers running in a dead network
// namespace because their `network_mode: container:<ID>` reference no
// longer resolves to a live container.
//
// This is the failure mode the anchord-v2 wrap pattern triggers when
// a service-anchor is recreated. compose resolves
// `network_mode: service:fe-anchor-X` to `container:<runtime-id>` at
// create-time and the dependent stays pinned across recreates. After
// fe-anchor-X is recreated, every dependent is in a netns Docker has
// destroyed — they look running but have no interface, no routes, no
// DNAT.
//
// See issue #9 for the 2026-05-23 production incident: a routine
// `docker compose up -d --no-deps --force-recreate` over 30 anchord
// containers silently orphaned 21 dependent containers.
package dependents

import "strings"

// Container is the minimal shape the predicate needs. Mirrors the
// shape internal/autostart.ContainerInfo uses to keep the test
// surface narrow and the package free of docker-client deps.
type Container struct {
	ID          string
	Names       []string
	State       string
	NetworkMode string
	Labels      map[string]string
}

// StaleNetns is one detected victim — a dependent container whose
// network_mode: container:<ID> reference does not resolve in the
// current container set.
type StaleNetns struct {
	Container   Container
	StaleTarget string // the unresolved ref (the dead container ID/name from network_mode)
	ComposeHint string // "docker compose -p <p> up -d --no-deps --force-recreate <svc>" when compose labels present, else ""
}

// Find returns every dependent in `candidates` whose
// network_mode:container:<X> reference does not resolve in `all`.
// `all` should be the FULL container list (typically with All=true)
// so the predicate doesn't false-positive on dependents pointing at
// stopped-but-still-extant containers. `candidates` is the scoped
// subset the caller wants reports for (anchord daemon: its own
// project; doctor CLI: everything).
//
// The two slices may safely be the same value — the predicate is
// purely lookup, no mutation.
func Find(candidates, all []Container) []StaleNetns {
	var out []StaleNetns
	for _, c := range candidates {
		ref, ok := strings.CutPrefix(c.NetworkMode, "container:")
		if !ok {
			continue
		}
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if refResolves(ref, all) {
			continue
		}
		out = append(out, StaleNetns{
			Container:   c,
			StaleTarget: ref,
			ComposeHint: composeHint(c),
		})
	}
	return out
}

// refResolves reports whether `ref` matches some container in `all`,
// in any of the three forms docker accepts for
// `network_mode: container:<...>`: full ID, short ID (>=12 chars),
// or name (with or without the leading slash docker prepends).
func refResolves(ref string, all []Container) bool {
	refIsShort := len(ref) >= 12 && len(ref) < 64
	for _, c := range all {
		if c.ID == ref {
			return true
		}
		if refIsShort && strings.HasPrefix(c.ID, ref) {
			return true
		}
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == ref {
				return true
			}
		}
	}
	return false
}

// composeHint returns the exact command an operator runs to recover.
// Falls back to "" when the dependent has no compose labels —
// non-compose containers need operator-specific recovery (`docker
// run` parameters etc.) and a generic hint would be misleading.
func composeHint(c Container) string {
	project := c.Labels["com.docker.compose.project"]
	svc := c.Labels["com.docker.compose.service"]
	if project == "" || svc == "" {
		return ""
	}
	return "docker compose -p " + project + " up -d --no-deps --force-recreate " + svc
}

// FirstName returns the cleanest human label for a container — first
// name with the leading "/" stripped, else short ID. Exported so the
// CLI and the Watcher logger render victims the same way.
func FirstName(c Container) string {
	for _, n := range c.Names {
		if n != "" {
			return strings.TrimPrefix(n, "/")
		}
	}
	if len(c.ID) >= 12 {
		return c.ID[:12]
	}
	return c.ID
}
