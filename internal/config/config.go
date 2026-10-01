// Package config loads ~/.config/ursa-bifrost/config.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// GCloud describes a login node reached through `gcloud compute ssh --tunnel-through-iap`.
type GCloud struct {
	Instance string `yaml:"instance"`
	Zone     string `yaml:"zone"`
	Project  string `yaml:"project"`
}

// SSH says how to reach the login node. Exactly one of GCloud or Host is used.
type SSH struct {
	GCloud         *GCloud `yaml:"gcloud,omitempty"`
	Host           string  `yaml:"host,omitempty"` // plain ssh host or alias from ~/.ssh/config
	ControlPersist int     `yaml:"control_persist"`
	ConnectTimeout int     `yaml:"connect_timeout"`
}

// TTL holds cache lifetimes per data kind.
type TTL struct {
	Queue   Duration `yaml:"queue"`
	Nodes   Duration `yaml:"nodes"`
	Acct    Duration `yaml:"acct"`
	Catalog Duration `yaml:"catalog"`
	Modules Duration `yaml:"modules"`
}

// Limits caps output sizes.
type Limits struct {
	LogLines    int `yaml:"log_lines"`
	ListRows    int `yaml:"list_rows"`
	ScriptBytes int `yaml:"script_bytes"`
	UntrustedCh int `yaml:"untrusted_chars"`
	CallsPerMin int `yaml:"calls_per_min"`
}

// Config is the whole file.
type Config struct {
	Cluster     string             `yaml:"cluster"`
	ClusterUser string             `yaml:"cluster_user,omitempty"` // default: `id -un` on the login node
	Backend     string             `yaml:"backend"`                // ssh | fixture
	FixturesDir string             `yaml:"fixtures_dir,omitempty"`
	SSH         SSH                `yaml:"ssh"`
	CatalogPath string             `yaml:"catalog_path"`
	LogRoots    []string           `yaml:"log_roots"` // allowed log prefixes; {user} expands
	Costs       map[string]float64 `yaml:"usd_per_node_hour,omitempty"`
	ShowCost    bool               `yaml:"show_cost"`
	Tiers       []string           `yaml:"tiers"` // tiers granted to the local user
	TTL         TTL                `yaml:"cache_ttl"`
	Limits      Limits             `yaml:"limits"`
	AuditPath   string             `yaml:"audit_path"`
	Path        string             `yaml:"-"`
}

// Duration is a time.Duration that reads "20s" / "10m" in YAML.
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("duration %q: %w", n.Value, err)
	}
	d.Duration = v
	return nil
}

// MarshalYAML writes the duration as a string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Default returns a configuration with every default filled in (no SSH target).
func Default() Config {
	return Config{
		Cluster:     "ursa-major",
		Backend:     "ssh",
		SSH:         SSH{ControlPersist: 900, ConnectTimeout: 30},
		CatalogPath: "/apps/docs/catalog.json",
		LogRoots:    []string{"/home/{user}/", "/scratch/{user}/"},
		ShowCost:    true,
		Tiers:       []string{"R1"},
		TTL: TTL{
			Queue:   Duration{20 * time.Second},
			Nodes:   Duration{30 * time.Second},
			Acct:    Duration{60 * time.Second},
			Catalog: Duration{time.Hour},
			Modules: Duration{time.Hour},
		},
		Limits: Limits{
			LogLines:    200,
			ListRows:    200,
			ScriptBytes: 64 * 1024,
			UntrustedCh: 16000,
			CallsPerMin: 60,
		},
		AuditPath: "~/.local/share/ursa-bifrost/audit.jsonl",
	}
}

// DefaultPath is ~/.config/ursa-bifrost/config.yaml (XDG_CONFIG_HOME honoured).
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "ursa-bifrost", "config.yaml")
}

// ErrNoConfig means the config file does not exist yet.
var ErrNoConfig = errors.New("no config file")

// Load reads path (or the default path) over the defaults.
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	c := Default()
	c.Path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("%w at %s (run `bifrost config init`)", ErrNoConfig, path)
	}
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, c.Validate()
}

// Validate checks the fields that would otherwise fail later and obscurely.
func (c Config) Validate() error {
	switch c.Backend {
	case "ssh":
		if c.SSH.GCloud == nil && c.SSH.Host == "" {
			return errors.New("config: ssh backend needs ssh.gcloud or ssh.host")
		}
		if g := c.SSH.GCloud; g != nil && (g.Instance == "" || g.Zone == "" || g.Project == "") {
			return errors.New("config: ssh.gcloud needs instance, zone and project")
		}
	case "fixture":
		if c.FixturesDir == "" {
			return errors.New("config: fixture backend needs fixtures_dir")
		}
	default:
		return fmt.Errorf("config: unknown backend %q (ssh or fixture)", c.Backend)
	}
	for _, t := range c.Tiers {
		switch t {
		case "R1", "R2", "A1":
		default:
			return fmt.Errorf("config: unknown tier %q (R1, R2, A1)", t)
		}
	}
	return nil
}

// HasTier reports whether the local user holds a tier.
func (c Config) HasTier(t string) bool {
	for _, x := range c.Tiers {
		if x == t {
			return true
		}
	}
	return false
}

// Expand replaces a leading ~ with the home directory.
func Expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Example is the file `bifrost config init` writes.
const Example = `# ursa-bifrost configuration
cluster: ursa-major
backend: ssh              # ssh | fixture

ssh:
  # Login node through Google IAP (same route deep-research uses)
  gcloud:
    instance: ucrslurmcl-slurm-login-001
    zone: us-central1-a
    project: ucr-ursa-major-hpc-cluster
  # host: ursa-login      # or a plain ssh host / ~/.ssh/config alias instead of gcloud
  control_persist: 900
  connect_timeout: 30

# Published by the cluster (ursa-catalog): partitions, prices, modules, recipes
catalog_path: /apps/docs/catalog.json

# Job logs may only be read under these prefixes ({user} = your cluster user)
log_roots: ["/home/{user}/", "/scratch/{user}/"]

# Dollar figures in usage and status output (prices come from the catalog;
# override per partition below)
show_cost: true
# usd_per_node_hour: {computehigh: 1.87}

# Tiers for the local user: R1 own jobs + cluster facts, R2 staff (all users),
# A1 submit/cancel (not implemented yet)
tiers: [R1]

cache_ttl: {queue: 20s, nodes: 30s, acct: 60s, catalog: 1h, modules: 1h}
limits: {log_lines: 200, list_rows: 200, script_bytes: 65536, untrusted_chars: 16000, calls_per_min: 60}
audit_path: ~/.local/share/ursa-bifrost/audit.jsonl
`
