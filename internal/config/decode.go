// Package config loads and validates client and node configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"

	"go.yaml.in/yaml/v3"
)

var unknownFieldRe = regexp.MustCompile(`line (\d+): field (\S+) not found in type (\S+)`)

// decodeStrict decodes YAML into v, rejecting unknown keys with a readable
// message.
func decodeStrict(b []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty file: defaults
		}
		var te *yaml.TypeError
		if errors.As(err, &te) {
			var msgs []string
			for _, e := range te.Errors {
				if m := unknownFieldRe.FindStringSubmatch(e); m != nil {
					msgs = append(msgs, fmt.Sprintf("line %s: unknown key %q (check spelling and indentation)", m[1], m[2]))
				} else {
					msgs = append(msgs, e)
				}
			}
			return fmt.Errorf("invalid config:\n  %s", joinLines(msgs))
		}
		return fmt.Errorf("invalid YAML: %w", err)
	}
	return nil
}

func joinLines(s []string) string {
	var buf bytes.Buffer
	for i, l := range s {
		if i > 0 {
			buf.WriteString("\n  ")
		}
		buf.WriteString(l)
	}
	return buf.String()
}
