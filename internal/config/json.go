package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// decodeFile shares the YAML field names and duration syntax with JSON.
// A .json file must contain exactly one JSON object. YAML's strict decoder
// then enforces the configuration schema and rejects duplicate keys.
func decodeFile(path string, data []byte, cfg *Config) error {
	trimmed := bytes.TrimSpace(data)
	if strings.EqualFold(filepath.Ext(path), ".json") || json.Valid(data) {
		if !json.Valid(data) || len(trimmed) == 0 || trimmed[0] != '{' {
			return fmt.Errorf("expected a single JSON object")
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return err
		}
		if err := checkJSONTypes(doc.Content[0], reflect.TypeFor[Config](), "config"); err != nil {
			return err
		}
	}
	return decodeYAML(data, cfg)
}

// YAML accepts some coercions (for example floats to integers) that are
// surprising in JSON. Check native JSON types against the shared schema
// before decoding, including nested storage backings and duration strings.
func checkJSONTypes(n *yaml.Node, typ reflect.Type, path string) error {
	if typ.Kind() == reflect.Pointer {
		// An optional block (e.g. "s3"): null leaves it unset.
		if n.Tag == "!!null" {
			return nil
		}
		return checkJSONTypes(n, typ.Elem(), path)
	}
	want := ""
	if typ == reflect.TypeFor[time.Duration]() {
		want = "!!str"
	} else {
		switch typ.Kind() {
		case reflect.Struct:
			want = "!!map"
			if n.Tag == want {
				fields := map[string]reflect.Type{}
				for i := 0; i < typ.NumField(); i++ {
					f := typ.Field(i)
					fields[strings.Split(f.Tag.Get("yaml"), ",")[0]] = f.Type
				}
				for i := 0; i < len(n.Content); i += 2 {
					key := n.Content[i].Value
					field, ok := fields[key]
					if !ok {
						return fmt.Errorf("%s: unknown key %q", path, key)
					}
					if err := checkJSONTypes(n.Content[i+1], field, path+"."+key); err != nil {
						return err
					}
				}
			}
		case reflect.Slice:
			want = "!!seq"
			if n.Tag == want {
				for i, child := range n.Content {
					if err := checkJSONTypes(child, typ.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
						return err
					}
				}
			}
		case reflect.String:
			want = "!!str"
		case reflect.Bool:
			want = "!!bool"
		case reflect.Int, reflect.Int64:
			want = "!!int"
		}
	}
	if n.Tag != want {
		return fmt.Errorf("%s: expected %s, got %s", path, strings.TrimPrefix(want, "!!"), strings.TrimPrefix(n.Tag, "!!"))
	}
	return nil
}

// JSON renders the static configuration using the same snake_case keys,
// string durations and numeric byte sizes as YAML. Secrets are included:
// call Redacted first for display. Runtime API JSON remains unchanged.
func (c Config) JSON() ([]byte, error) {
	data, err := c.YAML()
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := yaml.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	data, err = json.MarshalIndent(fields, "", "  ")
	return append(data, '\n'), err
}

// LoadSettings uses persisted UI changes only when management is enabled.
// S3-only servers neither read nor modify an existing settings.json.
func (c Config) LoadSettings() (*Store, error) {
	if !c.Admin.Enabled {
		return NewStore(SettingsFrom(c), ""), nil
	}
	return LoadStore(SettingsFrom(c), c.SettingsPath())
}
