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

// IAPConfig is the login node reached with per-user IAP + OS Login (no gcloud
// binary). Token comes from TokenCommand (laptop: `gcloud auth
// print-access-token`) or, in the HTTP server, from the signed-in user.
type IAPConfig struct {
	Project      string   `yaml:"project"`
	Zone         string   `yaml:"zone"`
	Instance     string   `yaml:"instance"`
	Email        string   `yaml:"email,omitempty"`         // laptop mode: whose token; default `gcloud config get-value account`
	TokenCommand []string `yaml:"token_command,omitempty"` // laptop mode
	HostKeys     []string `yaml:"host_keys,omitempty"`     // pinned login-node host keys (authorized_keys format)
	IdleMinutes  int      `yaml:"idle_minutes,omitempty"`
}

// ServerConfig is used only by `bifrost serve` (the hosted MCP server).
type ServerConfig struct {
	Listen    string `yaml:"listen"`     // default :8080 (PORT env wins on Cloud Run)
	BaseURL   string `yaml:"base_url"`   // public https URL, e.g. https://bifrost-xyz.run.app
	DataDir   string `yaml:"data_dir"`   // sessions, keys, per-user ledgers
	UsersFile string `yaml:"users_file"` // who may sign in and with which tiers
	// Google OAuth client (Web application) used for sign-in.
	GoogleClientID        string `yaml:"google_client_id"`
	GoogleClientSecretEnv string `yaml:"google_client_secret_env"` // env var holding the secret
	// SecretKeyEnv names an env var with 32 random bytes (base64) used to
	// encrypt stored Google refresh tokens and SSH keys at rest.
	SecretKeyEnv string `yaml:"secret_key_env"`
	// AccessTokenMinutes is the lifetime of bifrost access tokens (default 60).
	AccessTokenMinutes int `yaml:"access_token_minutes"`
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

// Caps bound what the A1 tier may start. A plan over any cap is refused.
type Caps struct {
	MaxNodes          int     `yaml:"max_nodes"`
	MaxHours          float64 `yaml:"max_hours"`
	MaxCostPerJobUSD  float64 `yaml:"max_cost_usd_per_job"`
	MaxCostPerDayUSD  float64 `yaml:"max_cost_usd_per_day"`
	MaxSubmitsPerDay  int     `yaml:"max_submits_per_day"`
	ConfirmTTLMinutes int     `yaml:"confirm_ttl_minutes"`
}

// Staging is the private Cloud Storage staging area for file uploads and
// download links (SPEC section 18). Empty Bucket = the file tools are off.
type Staging struct {
	Bucket         string `yaml:"bucket"`
	SignAs         string `yaml:"sign_as,omitempty"` // service account that signs links (Cloud Run: discovered)
	Token          string `yaml:"token,omitempty"`   // metadata | gcloud (default: metadata on Cloud Run, else gcloud)
	UploadMinutes  int    `yaml:"upload_minutes"`
	LinkMinutes    int    `yaml:"link_minutes"`
	MaxUploadBytes int64  `yaml:"max_upload_bytes"`
	MaxUserBytes   int64  `yaml:"max_user_bytes"`
	MaxLinkBytes   int64  `yaml:"max_link_bytes"`
	RetainDays     int    `yaml:"retain_days"` // must match the bucket's delete rule
}

// Config is the whole file.
type Config struct {
	Cluster     string             `yaml:"cluster"`
	ClusterUser string             `yaml:"cluster_user,omitempty"` // default: `id -un` on the login node
	Backend     string             `yaml:"backend"`                // ssh | iap | fixture
	FixturesDir string             `yaml:"fixtures_dir,omitempty"`
	SSH         SSH                `yaml:"ssh"`
	IAP         IAPConfig          `yaml:"iap"`
	Server      ServerConfig       `yaml:"server"`
	CatalogPath string             `yaml:"catalog_path"`
	LogRoots    []string           `yaml:"log_roots"` // allowed log prefixes; {user} expands
	Costs       map[string]float64 `yaml:"usd_per_node_hour,omitempty"`
	ShowCost    bool               `yaml:"show_cost"`
	Tiers       []string           `yaml:"tiers"` // tiers granted to the local user
	TTL         TTL                `yaml:"cache_ttl"`
	Limits      Limits             `yaml:"limits"`
	AuditPath   string             `yaml:"audit_path"`
	Caps        Caps               `yaml:"caps"`
	StatePath   string             `yaml:"state_path"`  // A1 plans, tokens and spend ledger
	ResultsDir  string             `yaml:"results_dir"` // local folder for downloaded results
	JobsRoot    string             `yaml:"jobs_root"`   // remote folder under $HOME for submitted jobs
	Staging     Staging            `yaml:"staging"`
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
			LogLines:    1000,
			ListRows:    200,
			ScriptBytes: 64 * 1024,
			UntrustedCh: 16000,
			CallsPerMin: 60,
		},
		AuditPath:  "~/.local/share/ursa-bifrost/audit.jsonl",
		StatePath:  "~/.local/share/ursa-bifrost/a1.json",
		ResultsDir: "~/ursa-results",
		JobsRoot:   "bifrost-jobs",
		Caps: Caps{MaxNodes: 4, MaxHours: 24, MaxCostPerJobUSD: 25, MaxCostPerDayUSD: 50,
			MaxSubmitsPerDay: 20, ConfirmTTLMinutes: 10},
		Staging: Staging{UploadMinutes: 15, LinkMinutes: 60, MaxUploadBytes: 5 << 30,
			MaxUserBytes: 20 << 30, MaxLinkBytes: 2 << 30, RetainDays: 7},
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
	case "iap":
		if c.IAP.Project == "" || c.IAP.Zone == "" || c.IAP.Instance == "" {
			return errors.New("config: iap backend needs iap.project, iap.zone and iap.instance")
		}
	default:
		return fmt.Errorf("config: unknown backend %q (ssh, iap or fixture)", c.Backend)
	}
	if c.JobsRoot != "bifrost-jobs" {
		return errors.New("config: jobs_root must be bifrost-jobs (other folders are not supported yet)")
	}
	if c.Caps.MaxNodes < 1 || c.Caps.MaxHours <= 0 || c.Caps.MaxCostPerJobUSD <= 0 || c.Caps.MaxCostPerDayUSD <= 0 || c.Caps.ConfirmTTLMinutes < 1 {
		return errors.New("config: caps must all be positive")
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
# A1 submit/cancel/hold/release own jobs (two-step confirm, capped below)
tiers: [R1]

# A1 caps: a plan over any of these is refused (worst case = nodes x hours x price)
caps: {max_nodes: 4, max_hours: 24, max_cost_usd_per_job: 25, max_cost_usd_per_day: 50,
       max_submits_per_day: 20, confirm_ttl_minutes: 10}
results_dir: ~/ursa-results          # bifrost job results <id> downloads here
jobs_root: bifrost-jobs              # submitted jobs run in ~/bifrost-jobs/<stamp>-<name> on the cluster
state_path: ~/.local/share/ursa-bifrost/a1.json

cache_ttl: {queue: 20s, nodes: 30s, acct: 60s, catalog: 1h, modules: 1h}
limits: {log_lines: 200, list_rows: 200, script_bytes: 65536, untrusted_chars: 16000, calls_per_min: 60}
audit_path: ~/.local/share/ursa-bifrost/audit.jsonl
`
