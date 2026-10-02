package strategy

import (
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// composeEnvFileRefs lists every `env_file:` target a Compose file
// names, in first-seen order without duplicates. Compose allows a
// single string, a list of strings, or a list of {path, required}
// maps, on any service. Paths are returned cleaned and relative to the
// project directory. A target that points outside that directory
// (absolute, or climbing out with "..") or has an unsafe name cannot
// be captured through the project-folder helper mount; those are
// returned in outside instead so the manifest can say so loudly.
func composeEnvFileRefs(data []byte) (inside, outside []string, err error) {
	var doc struct {
		Services map[string]struct {
			EnvFile yaml.Node `yaml:"env_file"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("reading the compose file's YAML: %w", err)
	}
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		if safeRelativeComposePath(p) {
			inside = append(inside, path.Clean(p))
		} else {
			outside = append(outside, p)
		}
	}
	for _, svc := range doc.Services {
		n := svc.EnvFile
		switch n.Kind {
		case yaml.ScalarNode:
			add(n.Value)
		case yaml.SequenceNode:
			for _, item := range n.Content {
				switch item.Kind {
				case yaml.ScalarNode:
					add(item.Value)
				case yaml.MappingNode:
					for i := 0; i+1 < len(item.Content); i += 2 {
						if item.Content[i].Value == "path" {
							add(item.Content[i+1].Value)
						}
					}
				}
			}
		}
	}
	return inside, outside, nil
}

// safeRelativeComposePath reports whether p stays inside the project
// directory and every segment is a plain file name Ballast is willing
// to put on a shell argv.
func safeRelativeComposePath(p string) bool {
	if path.IsAbs(p) {
		return false
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return false
	}
	for _, seg := range strings.Split(c, "/") {
		if !composeFileNamePattern.MatchString(seg) {
			return false
		}
	}
	return true
}
