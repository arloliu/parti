package main

import (
	"fmt"
	"os"

	"github.com/arloliu/parti/v2/provision"
	"gopkg.in/yaml.v3"
)

// loadConfig reads the YAML file at path and unmarshals it into a
// provision.Config. Returns an error wrapping the underlying file or parse
// error; errors are classification-ready (callers map to exit 3).
func loadConfig(path string) (provision.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return provision.Config{}, fmt.Errorf("partictl: read config %q: %w", path, err)
	}

	var cfg provision.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return provision.Config{}, fmt.Errorf("partictl: parse config %q: %w", path, err)
	}

	return cfg, nil
}
