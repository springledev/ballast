package catalog

import (
	"fmt"
	"regexp"
	"strings"
)

// TemplateContext carries the values replacements are drawn from.
// Params fills {{ .Params.NAME }}; Services fills {{ .Services.NAME }};
// Discovery fills {{ .Discovery.NAME }} with values already resolved
// at job-run time (spec §6.1) by the caller — Resolve itself never
// talks to Docker/Proxmox.
type TemplateContext struct {
	Params    map[string]string
	Services  map[string]string
	Discovery map[string]string
}

// placeholderRE matches a whole template placeholder token. Only a strictly
// limited grammar is accepted (see lookup for details); anything else inside
// {{ }} is rejected — the spec forbids arbitrary expressions.
var placeholderRE = regexp.MustCompile(`\{\{\s*([^}]+?)\s*\}\}`)

// DeepCopy returns a deep copy of the entry so callers (e.g. tests, or a
// future engine) can Resolve modifications against a base entry.
func (e *Entry) DeepCopy() *Entry {
	c := *e
	c.Discovery = copyDiscoveryMap(e.Discovery)
	c.Parameters = append([]Parameter(nil), e.Parameters...)
	c.Backup.Dumps = make([]Dump, len(e.Backup.Dumps))
	for i, d := range e.Backup.Dumps {
		d.Command = append([]string(nil), d.Command...)
		d.SecretEnv = copyStringMap(d.SecretEnv)
		c.Backup.Dumps[i] = d
	}
	c.Backup.Files = append([]FileSpec(nil), e.Backup.Files...)
	c.Backup.Volumes = append([]VolumeSpec(nil), e.Backup.Volumes...)
	if e.Backup.StopStart != nil {
		ss := *e.Backup.StopStart
		ss.Containers = append([]string(nil), e.Backup.StopStart.Containers...)
		c.Backup.StopStart = &ss
	}
	if e.Backup.Exec != nil {
		ec := *e.Backup.Exec
		ec.Args = append([]string(nil), e.Backup.Exec.Args...)
		c.Backup.Exec = &ec
	}
	c.Verify.FilterStderr = append([]string(nil), e.Verify.FilterStderr...)
	c.Verify.Methods = append([]string(nil), e.Verify.Methods...)
	if sm := e.Verify.Smoke; sm != nil {
		cs := *sm
		if sm.HTTP != nil {
			h := *sm.HTTP
			cs.HTTP = &h
		}
		if sm.TCP != nil {
			t := *sm.TCP
			cs.TCP = &t
		}
		c.Verify.Smoke = &cs
	}
	return &c
}

// copyStringMap returns a copy of m, or nil for a nil/empty m.
func copyStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// copyDiscoveryMap returns a copy of m, or nil for a nil/empty m.
func copyDiscoveryMap(m map[string]DiscoveryRule) map[string]DiscoveryRule {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]DiscoveryRule, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// lookup resolves one placeholder expression to its value. The expression
// grammar is intentionally minimal: `.Params.name` or `.Services.name` and
// nothing else. Any other expression is a validation error.
func lookup(expr string, ctx TemplateContext) (string, error) {
	parts := strings.SplitN(expr, ".", 3)
	// ".Params.name" → ["", "Params", "name"]
	if len(parts) != 3 || parts[0] != "" || (parts[1] != "Params" && parts[1] != "Services" && parts[1] != "Discovery") || parts[2] == "" {
		return "", fmt.Errorf("unsupported template expression %q: only {{ .Params.NAME }}, {{ .Services.NAME }} and {{ .Discovery.NAME }} are allowed", expr)
	}
	name := parts[2]
	switch parts[1] {
	case "Services":
		if v, ok := ctx.Services[name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("template references unknown detected service %q", name)
	case "Discovery":
		if v, ok := ctx.Discovery[name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("template references unknown discovery value %q", name)
	}
	if v, ok := ctx.Params[name]; ok {
		return v, nil
	}
	return "", fmt.Errorf("template references unknown parameter %q", name)
}

// substitute replaces all placeholders in s.
func substitute(s string, ctx TemplateContext) (string, error) {
	var firstErr error
	out := placeholderRE.ReplaceAllStringFunc(s, func(m string) string {
		if firstErr != nil {
			return m
		}
		v, err := lookup(placeholderRE.FindStringSubmatch(m)[1], ctx)
		if err != nil {
			firstErr = err
			return m
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// ResolveTemplate substitutes {{ .Params.* }} / {{ .Services.* }} /
// {{ .Discovery.* }} placeholders in s against ctx. It is exported for
// callers outside this package that resolve a template string in
// isolation — namely a discovery rule's fallback (spec §6.1), which is
// evaluated by internal/detect before Entry.Resolve runs, since a
// resolved discovery value is itself an input to Resolve.
func ResolveTemplate(s string, ctx TemplateContext) (string, error) {
	return substitute(s, ctx)
}

// ParamsWithDefaults returns params plus the catalog default of every
// declared parameter absent from it (see withDefaults). It is exported
// so a caller that needs resolved parameter values before calling
// Resolve — namely discovery fallback resolution (spec §6.1), which
// must run first since its output feeds Resolve's own
// TemplateContext.Discovery — sees the same defaulted values Resolve
// itself would use.
func (e *Entry) ParamsWithDefaults(params map[string]string) map[string]string {
	return e.withDefaults(params)
}

// withDefaults returns params plus the catalog default of every declared
// parameter absent from it, without modifying params. A value the user
// supplied — even an empty one — always wins over the default.
func (e *Entry) withDefaults(params map[string]string) map[string]string {
	out := make(map[string]string, len(params)+len(e.Parameters))
	for k, v := range params {
		out[k] = v
	}
	for _, p := range e.Parameters {
		if _, ok := out[p.Name]; !ok && p.Default != "" {
			out[p.Name] = p.Default
		}
	}
	return out
}

// missingRequiredParams lists catalog-declared required parameters absent
// from params.
func (e *Entry) missingRequiredParams(params map[string]string) []string {
	var missing []string
	for _, p := range e.Parameters {
		if p.Required {
			if _, ok := params[p.Name]; !ok {
				missing = append(missing, p.Name)
			}
		}
	}
	return missing
}

// Resolve substitutes every allowed template placeholder in template-bearing
// fields (dump containers and commands, file paths, verify image) and
// validates that all required parameters were supplied. A parameter the
// caller left out takes its catalog default, when it has one.
//
// Only simple string substitution of {{ .Params.* }} and {{ .Services.* }} is
// supported (spec §6 schema rule); unknown expressions cause an error, never
// Go template execution.
func (e *Entry) Resolve(ctx TemplateContext) error {
	ctx.Params = e.withDefaults(ctx.Params)
	if missing := e.missingRequiredParams(ctx.Params); len(missing) > 0 {
		return fmt.Errorf("missing required parameter(s): %s", strings.Join(missing, ", "))
	}

	for i, d := range e.Backup.Dumps {
		c, err := substitute(d.Container, ctx)
		if err != nil {
			return fmt.Errorf("dump %q container: %w", d.Name, err)
		}
		e.Backup.Dumps[i].Container = c
		for j, part := range d.Command {
			cs, err := substitute(part, ctx)
			if err != nil {
				return fmt.Errorf("dump %q command element %d: %w", d.Name, j, err)
			}
			e.Backup.Dumps[i].Command[j] = cs
		}
	}
	for i, f := range e.Backup.Files {
		p, err := substitute(f.Path, ctx)
		if err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
		e.Backup.Files[i].Path = p
	}
	for i, v := range e.Backup.Volumes {
		n, err := substitute(v.Name, ctx)
		if err != nil {
			return fmt.Errorf("volume %q: %w", v.Name, err)
		}
		e.Backup.Volumes[i].Name = n
	}
	ss := e.Backup.StopStart
	if ss != nil {
		for i, c := range ss.Containers {
			cs, err := substitute(c, ctx)
			if err != nil {
				return fmt.Errorf("stopStart container %d: %w", i, err)
			}
			ss.Containers[i] = cs
		}
	}
	if e.Verify.Image != "" {
		v, err := substitute(e.Verify.Image, ctx)
		if err != nil {
			return fmt.Errorf("verify image: %w", err)
		}
		e.Verify.Image = v
	}
	if sm := e.Verify.Smoke; sm != nil {
		c, err := substitute(sm.Container, ctx)
		if err != nil {
			return fmt.Errorf("verify smoke container: %w", err)
		}
		sm.Container = c
		if sm.HTTP != nil && sm.HTTP.Path != "" {
			p, err := substitute(sm.HTTP.Path, ctx)
			if err != nil {
				return fmt.Errorf("verify smoke http path: %w", err)
			}
			sm.HTTP.Path = p
		}
	}
	return nil
}

// lookupSecret resolves one {{ .Secrets.NAME }} placeholder expression
// against secrets. Unlike lookup, no other table is accepted here:
// secretEnv exists specifically to carry secret values, so mixing in
// a parameter or detected-container reference would only invite a
// catalog author to reach for {{ .Params.* }} here instead of the
// intended secret, out of habit.
func lookupSecret(expr string, secrets map[string]string) (string, error) {
	parts := strings.SplitN(expr, ".", 3)
	if len(parts) != 3 || parts[0] != "" || parts[1] != "Secrets" || parts[2] == "" {
		return "", fmt.Errorf("unsupported secretEnv template expression %q: only {{ .Secrets.NAME }} is allowed", expr)
	}
	if v, ok := secrets[parts[2]]; ok {
		return v, nil
	}
	return "", fmt.Errorf("secretEnv references unknown secret %q", parts[2])
}

// substituteSecret replaces every {{ .Secrets.NAME }} placeholder in s.
func substituteSecret(s string, secrets map[string]string) (string, error) {
	var firstErr error
	out := placeholderRE.ReplaceAllStringFunc(s, func(m string) string {
		if firstErr != nil {
			return m
		}
		v, err := lookupSecret(placeholderRE.FindStringSubmatch(m)[1], secrets)
		if err != nil {
			firstErr = err
			return m
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// ResolveSecrets substitutes {{ .Secrets.NAME }} placeholders in every
// backup.dumps[].secretEnv value, using plaintext secret values the
// caller has already read from a root-only credential file (spec §8;
// this method never touches a file itself, so it has nothing to leak
// on its own). Call this only immediately before actually running the
// dump command: a read-only caller that never executes a dump (e.g.
// re-verifying an existing snapshot) must not call it and must not
// need secret values to resolve the rest of the entry — Resolve
// already rejects any {{ .Secrets.* }} reference outside secretEnv,
// so those call sites are unaffected by secrets being absent.
func (e *Entry) ResolveSecrets(secrets map[string]string) error {
	for i, d := range e.Backup.Dumps {
		if len(d.SecretEnv) == 0 {
			continue
		}
		resolved := make(map[string]string, len(d.SecretEnv))
		for key, tmpl := range d.SecretEnv {
			v, err := substituteSecret(tmpl, secrets)
			if err != nil {
				return fmt.Errorf("dump %q secretEnv %q: %w", d.Name, key, err)
			}
			resolved[key] = v
		}
		e.Backup.Dumps[i].SecretEnv = resolved
	}
	return nil
}
