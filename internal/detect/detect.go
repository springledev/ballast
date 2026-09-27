// Package detect finds running Docker containers and suggests which
// bundled/user catalog entries they match, for the first-run wizard
// (spec §5.3.6: "detected Docker services → pick one (Immich
// suggested)"). It shells out to `docker` (AGENTS.md rule 4: wrap,
// don't reimplement) and never inspects container internals beyond
// name and image. Suggestions are made by matching container images
// against catalog entries' detect.docker.imagePattern using
// case-insensitive regular expressions.
package detect

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/runner"
)

// Container is one running Docker container, as much as detection
// needs to know about it.
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// ListRunning lists every currently running Docker container. An
// error here (commonly: Docker not installed, or the daemon not
// reachable) is the caller's to report — detection is always a
// best-effort convenience, never a hard requirement (a service can
// always be added manually).
func ListRunning(ctx context.Context, r *runner.Runner) ([]Container, error) {
	res, err := r.Run(ctx, nil, nil, runner.Command{
		Name: "docker",
		Args: []string{"ps", "--format", "{{.Names}}\t{{.Image}}"},
	})
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("docker is not installed, or not on $PATH: %w", err)
		}
		return nil, fmt.Errorf("listing running Docker containers: %w", err)
	}
	out := []Container{}
	for _, line := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			continue
		}
		out = append(out, Container{Name: fields[0], Image: fields[1]})
	}
	return out, nil
}

// ResolveDiscovery resolves every catalog.DiscoveryRule in discovery to
// its {{ .Discovery.<name> }} value (spec §6.1), for a caller (the
// pipeline) to feed into catalog.TemplateContext.Discovery before
// calling Entry.Resolve. params must already carry the entry's
// defaulted parameter values (catalog.Entry.ParamsWithDefaults) so a
// fallback referencing {{ .Params.* }} sees the same values Resolve
// itself would use.
//
// Today the only supported rule type is "docker-label": match is a
// "label=value" filter passed to `docker ps --filter`. Exactly one
// running container must match; zero matches fall back to the rule's
// declared fallback template (resolved against params only), and more
// than one match is ambiguous. Both are hard errors — resolution
// failure MUST fail the job loudly, and Ballast MUST NOT silently fall
// back past the entry's own declared fallback (spec §6.1).
func ResolveDiscovery(ctx context.Context, r *runner.Runner, dockerBin string, discovery map[string]catalog.DiscoveryRule, params map[string]string) (map[string]string, error) {
	if len(discovery) == 0 {
		return nil, nil
	}
	if r == nil {
		return nil, fmt.Errorf("container discovery is unavailable: no command runner is configured")
	}
	if dockerBin == "" {
		dockerBin = "docker"
	}
	names := make([]string, 0, len(discovery))
	for name := range discovery {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]string, len(discovery))
	for _, name := range names {
		rule := discovery[name]
		if rule.Type != "docker-label" {
			return nil, fmt.Errorf("discovery %q: unsupported type %q (only docker-label is supported)", name, rule.Type)
		}
		if rule.Match == "" {
			return nil, fmt.Errorf("discovery %q: match must not be empty", name)
		}
		matches, err := matchByLabel(ctx, r, dockerBin, rule.Match)
		if err != nil {
			return nil, fmt.Errorf("discovery %q: %w", name, err)
		}
		switch len(matches) {
		case 1:
			out[name] = matches[0]
		case 0:
			if rule.Fallback == "" {
				return nil, fmt.Errorf("discovery %q: no running container matched label %q, and no fallback is declared", name, rule.Match)
			}
			v, err := catalog.ResolveTemplate(rule.Fallback, catalog.TemplateContext{Params: params})
			if err != nil {
				return nil, fmt.Errorf("discovery %q: fallback: %w", name, err)
			}
			if v == "" {
				return nil, fmt.Errorf("discovery %q: no running container matched label %q, and the fallback resolved to an empty value", name, rule.Match)
			}
			out[name] = v
		default:
			return nil, fmt.Errorf("discovery %q: label %q matched %d running containers (%s); it must match exactly one", name, rule.Match, len(matches), strings.Join(matches, ", "))
		}
	}
	return out, nil
}

// ResolveDiscoveryOffline is ResolveDiscovery for callers that must
// not depend on the service's containers being up right now — adding
// a service, or re-verifying an existing snapshot. It tries Docker
// first; if that fails for any reason (Docker down, container
// stopped), it resolves every rule from its declared fallback alone.
// If any rule has no usable fallback, the original Docker error is
// returned.
func ResolveDiscoveryOffline(ctx context.Context, r *runner.Runner, dockerBin string, discovery map[string]catalog.DiscoveryRule, params map[string]string) (map[string]string, error) {
	out, liveErr := ResolveDiscovery(ctx, r, dockerBin, discovery, params)
	if liveErr == nil {
		return out, nil
	}
	out = make(map[string]string, len(discovery))
	for name, rule := range discovery {
		if rule.Fallback == "" {
			return nil, liveErr
		}
		v, err := catalog.ResolveTemplate(rule.Fallback, catalog.TemplateContext{Params: params})
		if err != nil || v == "" {
			return nil, liveErr
		}
		out[name] = v
	}
	return out, nil
}

// matchByLabel lists the names of running containers whose labels
// match a "label=value" `docker ps --filter` expression.
func matchByLabel(ctx context.Context, r *runner.Runner, dockerBin, match string) ([]string, error) {
	res, err := r.Run(ctx, nil, nil, runner.Command{
		Name: dockerBin,
		Args: []string{"ps", "--filter", "label=" + match, "--format", "{{.Names}}"},
	})
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("docker is not installed, or not on $PATH: %w", err)
		}
		return nil, fmt.Errorf("listing containers matching label %q: %w", match, err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// Suggestion pairs a catalog entry with the running container(s)
// whose image matched its detect.docker.imagePattern.
type Suggestion struct {
	Service    string      `json:"service"`
	Containers []Container `json:"containers"`
}

// Suggest matches every entry with a detect.docker rule against
// containers, by case-insensitive regular expression matching on the
// image name. Patterns are compiled with the (?i) flag for case
// insensitivity. If a pattern fails to compile as a regex, it is
// treated as a non-match. Entries with no detect.docker rule (e.g. the
// generic "folder" entry) are never suggested — they are always a
// manual pick, which is correct: nothing about a folder is
// auto-detectable.
//
// Suggestions are ordered most specific first — by the length of the
// matching imagePattern, ties by service name — so an app-specific
// entry ("ghcr.io/immich-app/immich-server") is offered ahead of a
// generic engine entry ("postgres") that also matches one of the
// app's own containers. The wizard offers the first suggestion.
func Suggest(entries []*catalog.Entry, containers []Container) []Suggestion {
	out := []Suggestion{}
	patternLen := map[string]int{}
	for _, e := range entries {
		if e.Detect.Docker == nil || e.Detect.Docker.ImagePattern == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + e.Detect.Docker.ImagePattern)
		if err != nil {
			// Pattern fails to compile as regex: skip this entry (treat as no match)
			continue
		}
		var matches []Container
		for _, c := range containers {
			if re.MatchString(c.Image) {
				matches = append(matches, c)
			}
		}
		if len(matches) > 0 {
			out = append(out, Suggestion{Service: e.Service, Containers: matches})
			patternLen[e.Service] = len(e.Detect.Docker.ImagePattern)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := patternLen[out[i].Service], patternLen[out[j].Service]
		if li != lj {
			return li > lj
		}
		return out[i].Service < out[j].Service
	})
	return out
}
