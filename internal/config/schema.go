package config

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
)

//go:generate go run ./gen-schemas ../../schemas

// Schemas returns the JSON Schemas for both config files, keyed by file name.
// The committed files in schemas/ must equal this output (enforced by tests).
func Schemas() (map[string][]byte, error) {
	out := map[string][]byte{}
	for name, gen := range map[string]func() (*jsonschema.Schema, error){
		"client-config.schema.json": func() (*jsonschema.Schema, error) { return jsonschema.For[ClientConfig](nil) },
		"node-config.schema.json":   func() (*jsonschema.Schema, error) { return jsonschema.For[NodeConfig](nil) },
	} {
		s, err := gen()
		if err != nil {
			return nil, err
		}
		stripRequired(s)
		s.Required = []string{"version"}
		s.Schema = "https://json-schema.org/draft/2020-12/schema"
		b, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return nil, err
		}
		out[name] = append(b, '\n')
	}
	return out, nil
}

// stripRequired removes "required" everywhere: every config key has a default.
func stripRequired(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	s.Required = nil
	for _, p := range s.Properties {
		stripRequired(p)
	}
	stripRequired(s.Items)
	for _, p := range s.PrefixItems {
		stripRequired(p)
	}
}
