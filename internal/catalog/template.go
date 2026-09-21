package catalog

import (
	"fmt"
	"regexp"
	"strings"
)

// TemplateContext carries the values replacements are drawn from.
// Params fills {{ .Params.NAME }}; Services fills {{ .Services.NAME }}.
type TemplateContext struct {
	Params   map[string]string
	Services map[string]string
}

// placeholderRE matches a whole template placeholder token. Only a strictly
// limited grammar is accepted (see lookup for details); anything else inside
// {{ }} is rejected — the spec forbids arbitrary expressions.
var placeholderRE = regexp.MustCompile(`\{\{\s*([^}]+?)\s*\}\}`)

// DeepCopy returns a deep copy of the entry so callers (e.g. tests, or a
// future engine) can Resolve modifications against a base entry.
func (e *Entry) DeepCopy() *Entry {
	c := *e
	c.Parameters = append([]Parameter(nil), e.Parameters...)
	c.Backup.Dumps = append([]Dump(nil), e.Backup.Dumps...)
	c.Backup.Files = append([]FileSpec(nil), e.Backup.Files...)
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
	return &c
}

// lookup resolves one placeholder expression to its value. The expression
// grammar is intentionally minimal: `.Params.name` or `.Services.name` and
// nothing else. Any other expression is a validation error.
func lookup(expr string, ctx TemplateContext) (string, error) {
	parts := strings.SplitN(expr, ".", 3)
	// ".Params.name" → ["", "Params", "name"]
	if len(parts) != 3 || parts[0] != "" || (parts[1] != "Params" && parts[1] != "Services") || parts[2] == "" {
		return "", fmt.Errorf("unsupported template expression %q: only {{ .Params.NAME }} and {{ .Services.NAME }} are allowed", expr)
	}
	table, name := ctx.Params, parts[2]
	if parts[1] == "Services" {
		table, name = ctx.Services, parts[2]
		if v, ok := table[name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("template references unknown detected service %q", name)
	}
	if v, ok := table[name]; ok {
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
// validates that all required parameters were supplied.
//
// Only simple string substitution of {{ .Params.* }} and {{ .Services.* }} is
// supported (spec §6 schema rule); unknown expressions cause an error, never
// Go template execution.
func (e *Entry) Resolve(ctx TemplateContext) error {
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
	return nil
}
