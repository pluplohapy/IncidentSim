package verifier

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ReplayProfile is the workload file of a task, e.g.
// workloads/db-connection-leak.yaml.
type ReplayProfile struct {
	// Profile is the profile name, echoed in the report.
	Profile string `yaml:"profile"`
	// Duration is the observation window, e.g. 30s.
	Duration string `yaml:"duration"`
	// RPS is the target request rate.
	RPS int `yaml:"rps"`
	// Concurrency is the number of parallel replayed requests.
	Concurrency int `yaml:"concurrency"`
	// Requests are the endpoints the replay hits.
	Requests []Request `yaml:"requests"`
}

// requestProfile is the YAML shape of one replayed request.
type requestProfile struct {
	Name   string `yaml:"name"`
	Method string `yaml:"method"`
	Path   string `yaml:"path"`
	Weight int    `yaml:"weight"`
}

// LoadReplayProfile reads a workload file relative to a catalog directory.
func LoadReplayProfile(catalogDir, profile string) (*ReplayProfile, error) {
	if profile == "" {
		return nil, fmt.Errorf("%w: empty profile path", ErrNoReplayProfile)
	}
	path := profile
	if !filepath.IsAbs(path) {
		path = filepath.Join(catalogDir, profile)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read replay profile %s: %w", profile, err)
	}
	profile2 := &ReplayProfile{}
	if err := yaml.Unmarshal(data, profile2); err != nil {
		return nil, fmt.Errorf("parse replay profile %s: %w", profile, err)
	}
	if err := profile2.validate(); err != nil {
		return nil, fmt.Errorf("replay profile %s: %w", profile, err)
	}
	return profile2, nil
}

// validate rejects profiles that cannot drive a replay.
func (p *ReplayProfile) validate() error {
	if p.Profile == "" {
		return fmt.Errorf("profile name is required")
	}
	if _, err := time.ParseDuration(p.Duration); err != nil {
		return fmt.Errorf("duration %q is not a duration", p.Duration)
	}
	if p.RPS <= 0 {
		return fmt.Errorf("rps must be positive, got %d", p.RPS)
	}
	if len(p.Requests) == 0 {
		return fmt.Errorf("at least one request is required")
	}
	for i, request := range p.Requests {
		if request.Path == "" {
			return fmt.Errorf("request %d has no path", i)
		}
		if request.Method == "" {
			p.Requests[i].Method = "GET"
		}
		if p.Requests[i].Weight <= 0 {
			p.Requests[i].Weight = 1
		}
	}
	return nil
}

// UnmarshalYAML normalises the weight field, which YAML spells the same in both
// shapes.
func (r *Request) UnmarshalYAML(value *yaml.Node) error {
	var raw requestProfile
	if err := value.Decode(&raw); err != nil {
		return err
	}
	r.Name = raw.Name
	r.Method = raw.Method
	r.Path = raw.Path
	r.Weight = raw.Weight
	return nil
}

// workloadLine parses a `weight:name:path` line of an inline replay profile.
// Inline profiles exist so a task can carry its replay inside the manifest's
// replay.profile field, which keeps a task in one file.
func workloadLine(text string) (Request, bool) {
	parts := strings.SplitN(text, ":", 3)
	if len(parts) < 3 {
		return Request{}, false
	}
	weight, err := strconv.Atoi(parts[0])
	if err != nil {
		return Request{}, false
	}
	return Request{Weight: weight, Name: parts[1], Path: parts[2]}, true
}
