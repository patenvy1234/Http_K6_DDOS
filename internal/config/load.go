package config

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadFile reads an attack profile from disk and unmarshals it.
// Validation is NOT performed here — callers must call Validate().
func LoadFile(path string) (*AttackProfile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var p AttackProfile
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	if err := decoder.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var trailingDocument any
	if err := decoder.Decode(&trailingDocument); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		return nil, fmt.Errorf("parse %s: expected exactly one YAML document", path)
	}
	return &p, nil
}

// LoadAndValidate is a convenience helper that loads + validates in one shot.
func LoadAndValidate(path string) (*AttackProfile, error) {
	p, err := LoadFile(path)
	if err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("validation failed for %s: %w", path, err)
	}
	return p, nil
}
