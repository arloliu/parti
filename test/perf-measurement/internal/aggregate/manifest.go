package aggregate

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Manifest is the minimal slice of the harness Manifest the aggregator
// needs. We don't import the harness package to keep the dependency
// graph clean.
type Manifest struct {
	Status string `yaml:"status"`
}

// ParseManifest reads manifest.yaml and returns its Status field.
func ParseManifest(path string) (Manifest, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("open manifest: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(buf, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Status == "" {
		return Manifest{}, errors.New("manifest: status field is empty")
	}

	return m, nil
}
