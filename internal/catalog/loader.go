package catalog

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"regexp"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed schema/catalog-v1.json
var schemaFS embed.FS

var (
	schemaOnce   sync.Once
	schemaErr    error
	schemaLoader *jsonschema.Schema
)

// compileSchema parses and compiles the embedded catalog-v1.json schema
// exactly once. The compiled schema is shared by all Load calls.
func compileSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		raw, err := schemaFS.ReadFile("schema/catalog-v1.json")
		if err != nil {
			schemaErr = fmt.Errorf("reading embedded catalog schema: %w", err)
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			schemaErr = fmt.Errorf("parsing catalog schema JSON: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("https://ballast.dev/schema/catalog-v1.json", doc); err != nil {
			schemaErr = fmt.Errorf("registering catalog schema resource: %w", err)
			return
		}
		sch, err := c.Compile("https://ballast.dev/schema/catalog-v1.json")
		if err != nil {
			schemaErr = fmt.Errorf("compiling catalog schema: %w", err)
			return
		}
		schemaLoader = sch
	})
	return schemaLoader, schemaErr
}

// LoadFile reads, validates, and parses a catalog YAML file (wrapped to
// include the path in any error).
func LoadFile(path string) (*Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading catalog file %s: %w", path, err)
	}
	entry, err := Load(data)
	if err != nil {
		return nil, fmt.Errorf("catalog file %s: %w", path, err)
	}
	return entry, nil
}

// Load validates raw catalog YAML against the catalog-v1 JSON schema and
// returns the parsed Entry. Validation errors include the YAML document's
// JSON pointer, so users see exactly where their file is wrong.
func Load(data []byte) (*Entry, error) {
	sch, err := compileSchema()
	if err != nil {
		return nil, err
	}

	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	jsonRaw, err := toJSONCompatible(raw)
	if err != nil {
		return nil, fmt.Errorf("converting YAML: %w", err)
	}
	if err := sch.Validate(jsonRaw); err != nil {
		return nil, fmt.Errorf("catalog validation: %w", err)
	}

	norm, err := yaml.Marshal(jsonRaw)
	if err != nil {
		return nil, fmt.Errorf("re-encoding validated catalog: %w", err)
	}
	entry := &Entry{}
	if err := yaml.Unmarshal(norm, entry); err != nil {
		return nil, fmt.Errorf("parsing validated catalog into types: %w", err)
	}
	if entry.Detect.Docker != nil && entry.Detect.Docker.ImagePattern != "" {
		if _, err := regexp.Compile("(?i)" + entry.Detect.Docker.ImagePattern); err != nil {
			return nil, fmt.Errorf("detect.docker.imagePattern %q is not a valid regular expression: %w", entry.Detect.Docker.ImagePattern, err)
		}
	}
	return entry, nil
}

// toJSONCompatible converts a yaml.v3-decoded value (map[any]any keyed by
// any) into JSON-friendly forms (map[string]any) so the JSON schema
// validator can walk it. yaml.v3 already decodes nested keys into map[string]any
// when possible, but this normalises any legacy map[any]any nodes defensively.
func toJSONCompatible(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			nv, err := toJSONCompatible(val)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			key, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("non-string map key %v", k)
			}
			nv, err := toJSONCompatible(val)
			if err != nil {
				return nil, err
			}
			out[key] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			nv, err := toJSONCompatible(val)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	default:
		// plain JSON-decodable scalars fall through untouched.
		return v, nil
	}
}
