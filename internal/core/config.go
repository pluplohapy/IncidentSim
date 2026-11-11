// Package core is the orchestrator of IncidentSim: it owns the task catalog,
// the session lifecycle, the event journal and the HTTP API that the CLI talks
// to. It is the only package that knows about both the manifest schema and the
// runtime adapters.
package core

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the deployment configuration of the core.
type Config struct {
	// Listen is the address the HTTP API binds to. It defaults to loopback:
	// the core is a local service, not an internet-facing one.
	Listen string `yaml:"listen"`
	// Catalog is the directory holding the task manifests.
	Catalog string `yaml:"catalog"`
	// State is the directory holding journals and scratch compose projects.
	State string `yaml:"state"`
	// Engine selects the runtime adapter: compose for local development or
	// firecracker on a Linux host with KVM.
	Engine string `yaml:"engine"`

	// path is the file the config was loaded from, for error messages.
	path string
}

// DefaultConfig returns the configuration of a development machine: compose
// engine, loopback listener, catalog and state under the working directory.
func DefaultConfig() Config {
	return Config{
		Listen:  "127.0.0.1:8747",
		Catalog: "manifests",
		State:   ".incidentsim",
		Engine:  "compose",
	}
}

// LoadConfig reads a configuration file. A missing file is not an error: the
// defaults must be enough to run `incidentsim serve` on a fresh checkout.
func LoadConfig(path string) (Config, error) {
	config := DefaultConfig()
	if path == "" {
		return config.withDefaults(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return config.withDefaults(), nil
		}
		return config, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("parse config %s: %w", path, err)
	}
	config.path = path
	return config.withDefaults(), nil
}

func (c Config) withDefaults() Config {
	if c.Listen == "" {
		c.Listen = DefaultConfig().Listen
	}
	if c.Catalog == "" {
		c.Catalog = DefaultConfig().Catalog
	}
	if c.State == "" {
		c.State = DefaultConfig().State
	}
	if c.Engine == "" {
		c.Engine = "compose"
	}
	return c
}

// Path returns the file the config was loaded from, or "" for defaults.
func (c Config) Path() string { return c.path }

// CatalogDir returns the absolute catalog directory.
func (c Config) CatalogDir() string { return absOrSelf(c.Catalog) }

// StateDir returns the absolute state directory.
func (c Config) StateDir() string { return absOrSelf(c.State) }

// JournalDir is where per-session event logs live.
func (c Config) JournalDir() string { return filepath.Join(c.StateDir(), "journal") }

// ProjectDir returns the root the manifest asset paths are relative to, i.e.
// the parent of the catalog directory: that is where workloads/ live.
func (c Config) ProjectDir() string { return filepath.Dir(c.CatalogDir()) }

func absOrSelf(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
