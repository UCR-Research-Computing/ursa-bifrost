package core

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Catalog is the cluster's published description (ursa-catalog/1 schema),
// read from catalog_path on the login node.
type Catalog struct {
	Schema     string             `json:"schema"`
	Cluster    string             `json:"cluster"`
	Generated  string             `json:"generated"`
	Partitions []CatalogPartition `json:"partitions"`
	Rules      []string           `json:"rules"`
	Modules    struct {
		Core         []string `json:"core"`
		MPIDependent map[string]struct {
			Requires string   `json:"requires"`
			Modules  []string `json:"modules"`
		} `json:"mpi_dependent"`
		ByPartition map[string][]string `json:"by_partition,omitempty"`
	} `json:"modules"`
	Recipes      []Recipe          `json:"recipes"`
	UsageCards   map[string]string `json:"usage_cards"`
	Containers   []Container       `json:"containers"`
	Datasets     Datasets          `json:"datasets"`
	InstallTools []InstallTool     `json:"install_tools"`
	JobHeader    struct {
		Path string `json:"path"`
		Use  string `json:"use"`
	} `json:"job_header"`
	HowToLoad    string          `json:"how_to_load"`
	ModuleHealth json.RawMessage `json:"module_health"`
	Summary      struct {
		GPU map[string]any `json:"gpu"`
	} `json:"summary"`
}

// CatalogPartition is one partition as described by the catalog.
type CatalogPartition struct {
	Name         string   `json:"name"`
	Default      bool     `json:"default"`
	MaxNodes     int      `json:"max_nodes"`
	CPUsPerNode  int      `json:"cpus_per_node"`
	MemGBPerNode float64  `json:"mem_gb_per_node"`
	GPUsPerNode  int      `json:"gpus_per_node"`
	GPUType      *string  `json:"gpu_type"`
	TimeLimit    string   `json:"time_limit"`
	State        string   `json:"state"`
	USDPerNodeHr *float64 `json:"usd_per_node_hour"`
	Spot         bool     `json:"spot"`
	UseFor       string   `json:"use_for"`
	// AllowedAccounts is Slurm's AllowAccounts for the partition, published by ursa-catalog
	// since 2026-11-01: nil/empty = open to every account; otherwise only these accounts
	// may submit (lab-only and Tier 1 partitions). v0.10.5.
	AllowedAccounts []string `json:"allowed_accounts,omitempty"`
	OnDemandBoots   bool     `json:"-"`
}

// Restricted reports whether only some Slurm accounts may use the partition.
func (p CatalogPartition) Restricted() bool { return len(p.AllowedAccounts) > 0 }

// Allows reports whether one of accounts may use the partition.
func (p CatalogPartition) Allows(accounts []string) bool {
	if !p.Restricted() {
		return true
	}
	for _, a := range accounts {
		for _, ok := range p.AllowedAccounts {
			if a == ok {
				return true
			}
		}
	}
	return false
}

// Recipe is a known-good way to run a package.
type Recipe struct {
	Name      string   `json:"name"`
	Field     string   `json:"field"`
	Load      []string `json:"load"`
	Run       string   `json:"run"`
	Partition string   `json:"partition"`
	GPUs      int      `json:"gpus,omitempty"`
	Notes     string   `json:"notes,omitempty"`
}

// Container is a prebuilt Apptainer image.
type Container struct {
	Path   string  `json:"path"`
	SizeGB float64 `json:"size_gb"`
}

// Dataset is one shared reference database in /data/shared (catalog "datasets").
type Dataset struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Path    string   `json:"path"`
	Module  string   `json:"module"`
	SizeGB  float64  `json:"size_gb,omitempty"`
	Env     []string `json:"env,omitempty"`
}

// Datasets is the catalog's shared reference-data section.
type Datasets struct {
	Root        string    `json:"root"`
	Mounted     bool      `json:"mounted"`
	SizeGB      float64   `json:"size_gb,omitempty"`
	FreeGB      float64   `json:"free_gb,omitempty"`
	ReadMiBPerS float64   `json:"read_mib_per_s,omitempty"` // cluster-wide read throughput (v0.9.12)
	Note        string    `json:"note,omitempty"`
	Items       []Dataset `json:"items"`
}

// SearchDatasets finds reference databases by name, db-* module or environment variable
// (case and separators ignored, so "kraken" finds kraken2-standard and "blastdb" finds
// every database that sets BLASTDB). "db-" alone lists them all.
func (c *Catalog) SearchDatasets(q string) []Dataset {
	norm := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == '-' || r == '_' || r == '.' || r == ' ' || r == '/' {
				return -1
			}
			return r
		}, strings.ToLower(s))
	}
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	rest := strings.TrimPrefix(q, "db-")
	if rest == "" {
		return append([]Dataset(nil), c.Datasets.Items...)
	}
	nq := norm(rest)
	if nq == "" {
		return nil
	}
	var out []Dataset
	for _, d := range c.Datasets.Items {
		for _, f := range append([]string{d.Name, d.Module}, d.Env...) {
			if strings.Contains(norm(f), nq) {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// InstallTool is a way to install software without root.
type InstallTool struct {
	Name      string `json:"name"`
	How       string `json:"how"`
	Use       string `json:"use"`
	Available bool   `json:"available"`
}

// Partition looks a partition up by name.
func (c *Catalog) Partition(name string) (CatalogPartition, bool) {
	for _, p := range c.Partitions {
		if p.Name == name {
			return p, true
		}
	}
	return CatalogPartition{}, false
}

// DefaultPartition is the partition marked default (or "").
func (c *Catalog) DefaultPartition() string {
	for _, p := range c.Partitions {
		if p.Default {
			return p.Name
		}
	}
	return ""
}

// ModuleHit is one module search result.
type ModuleHit struct {
	Name       string   `json:"name"`
	Versions   []string `json:"versions"`
	Requires   string   `json:"requires,omitempty"`   // e.g. "module load openmpi"
	Partitions []string `json:"partitions,omitempty"` // non-empty when mounted only on specific partitions (e.g. tpu*)
	GPU        bool     `json:"gpu,omitempty"`
	Card       string   `json:"usage_card,omitempty"`
}

// SearchModules finds modules whose name contains q (case-insensitive).
// An empty query lists everything.
func (c *Catalog) SearchModules(q string) []ModuleHit {
	q = strings.ToLower(strings.TrimSpace(q))
	byName := map[string]*ModuleHit{}
	add := func(full, requires string, parts []string) {
		name, ver, _ := strings.Cut(full, "/")
		if q != "" && !strings.Contains(strings.ToLower(full), q) {
			return
		}
		key := name + "|" + requires
		h := byName[key]
		if h == nil {
			h = &ModuleHit{Name: name, Requires: requires, Partitions: parts}
			byName[key] = h
		} else if len(parts) == 0 {
			h.Partitions = nil // also in Core/MPIDependent -> available cluster-wide
		}
		if ver != "" {
			seenVer := false
			for _, v := range h.Versions {
				if v == ver {
					seenVer = true
					break
				}
			}
			if !seenVer {
				h.Versions = append(h.Versions, ver)
			}
			if strings.Contains(ver, "cuda") {
				h.GPU = true
			}
		}
	}
	for _, m := range c.Modules.Core {
		add(m, "", nil)
	}
	mpis := make([]string, 0, len(c.Modules.MPIDependent))
	for k := range c.Modules.MPIDependent {
		mpis = append(mpis, k)
	}
	sort.Strings(mpis)
	for _, k := range mpis {
		g := c.Modules.MPIDependent[k]
		for _, m := range g.Modules {
			add(m, g.Requires, nil)
		}
	}
	partMap := map[string][]string{}
	for pName, list := range c.Modules.ByPartition {
		for _, m := range list {
			partMap[m] = append(partMap[m], pName)
		}
	}
	partMods := make([]string, 0, len(partMap))
	for m := range partMap {
		partMods = append(partMods, m)
	}
	sort.Strings(partMods)
	for _, m := range partMods {
		ps := partMap[m]
		sort.Strings(ps)
		add(m, "", ps)
	}
	out := make([]ModuleHit, 0, len(byName))
	for _, h := range byName {
		if card, ok := c.UsageCards[h.Name]; ok {
			h.Card = card
		}
		if h.Name == "python-ml" {
			h.GPU = true
		}
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Requires < out[j].Requires
	})
	return out
}

// fallbackTPUBases names the site TPU modulefiles mounted from /apps/modulefiles-tpu
// so older catalog snapshots without modules.by_partition still validate TPU jobs.
var fallbackTPUBases = map[string]bool{
	"python-tpu":  true,
	"pytorch-tpu": true,
}

// ModulePartitions returns the sorted list of partitions where spec is mounted when
// spec is partition-scoped (not in Core/MPIDependent).
func (c *Catalog) ModulePartitions(spec string) []string {
	name, ver, _ := strings.Cut(spec, "/")
	var ps []string
	seen := map[string]bool{}
	for pName, list := range c.Modules.ByPartition {
		for _, m := range list {
			n, v, _ := strings.Cut(m, "/")
			if n == name && (ver == "" || v == ver) && !seen[pName] {
				seen[pName] = true
				ps = append(ps, pName)
			}
		}
	}
	if len(ps) == 0 && fallbackTPUBases[name] {
		for _, p := range c.Partitions {
			if strings.HasPrefix(p.Name, "tpu") && !seen[p.Name] {
				seen[p.Name] = true
				ps = append(ps, p.Name)
			}
		}
		if len(ps) == 0 {
			ps = []string{"tpu", "tpu6spot", "tpuspot"}
		}
	}
	sort.Strings(ps)
	return ps
}

// ModuleExistsOnPartition reports whether spec is mounted on every partition in
// partitionSpec (a single name or Slurm comma list like "tpu6spot,tpuspot").
func (c *Catalog) ModuleExistsOnPartition(spec, partitionSpec string) (bool, string) {
	if ok, req := c.ModuleExists(spec); ok {
		return true, req
	}
	var parts []string
	for _, s := range strings.Split(partitionSpec, ",") {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return false, ""
	}
	allowed := map[string]bool{}
	for _, p := range c.ModulePartitions(spec) {
		allowed[p] = true
	}
	name, _, _ := strings.Cut(spec, "/")
	for _, p := range parts {
		if !allowed[p] && !(fallbackTPUBases[name] && strings.HasPrefix(p, "tpu")) {
			return false, ""
		}
	}
	return true, ""
}

// ModuleExists reports whether "name" or "name/version" is a known module,
// and which MPI module it needs first ("" when none).
func (c *Catalog) ModuleExists(spec string) (bool, string) {
	name, ver, _ := strings.Cut(spec, "/")
	match := func(full string) bool {
		n, v, _ := strings.Cut(full, "/")
		return n == name && (ver == "" || v == ver)
	}
	for _, m := range c.Modules.Core {
		if match(m) {
			return true, ""
		}
	}
	// bare module files without versions (apptainer, gromacs, openfoam) are only in
	// `module avail`, not the catalog list; known ones are accepted here.
	for _, bare := range []string{"apptainer", "gromacs", "openfoam"} {
		if name == bare && ver == "" {
			return true, ""
		}
	}
	if reqs := c.mpiRequires(match); len(reqs) > 0 {
		return true, reqs[0]
	}
	return false, ""
}

// ModuleRequires lists every MPI prerequisite ("module load openmpi", ...) under which
// spec is built, sorted; nil for core modules or unknown ones. A package built for
// several MPIs is satisfied by loading any one of them.
func (c *Catalog) ModuleRequires(spec string) []string {
	name, ver, _ := strings.Cut(spec, "/")
	return c.mpiRequires(func(full string) bool {
		n, v, _ := strings.Cut(full, "/")
		return n == name && (ver == "" || v == ver)
	})
}

func (c *Catalog) mpiRequires(match func(string) bool) []string {
	var out []string
	for _, g := range c.Modules.MPIDependent {
		for _, m := range g.Modules {
			if match(m) {
				out = append(out, g.Requires)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// SearchContainers finds prebuilt Apptainer images whose file name contains q
// (separators ignored, so "alphafold", "colabfold" and "py torch" all match).
func (c *Catalog) SearchContainers(q string) []Container {
	norm := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == '-' || r == '_' || r == '.' || r == ' ' {
				return -1
			}
			return r
		}, strings.ToLower(s))
	}
	nq := norm(q)
	if nq == "" {
		return nil
	}
	var out []Container
	for _, ct := range c.Containers {
		base := ct.Path[strings.LastIndexByte(ct.Path, '/')+1:]
		if strings.Contains(norm(base), nq) {
			out = append(out, ct)
		}
	}
	return out
}

// SearchRecipes finds recipes by name, field or module.
func (c *Catalog) SearchRecipes(q string) []Recipe {
	q = strings.ToLower(strings.TrimSpace(q))
	var out []Recipe
	for _, r := range c.Recipes {
		hay := strings.ToLower(r.Name + " " + r.Field + " " + strings.Join(r.Load, " ") + " " + r.Run)
		if q == "" || strings.Contains(hay, q) {
			out = append(out, r)
		}
	}
	return out
}

// Closest returns up to n known module names nearest to name (for "did you mean").
func (c *Catalog) Closest(name string, n int) []string {
	base, _, _ := strings.Cut(strings.ToLower(name), "/")
	type cand struct {
		s string
		d int
	}
	seen := map[string]bool{}
	var cs []cand
	consider := func(full string) {
		nm, _, _ := strings.Cut(full, "/")
		if seen[nm] {
			return
		}
		seen[nm] = true
		cs = append(cs, cand{nm, levenshtein(base, strings.ToLower(nm))})
	}
	for _, m := range c.Modules.Core {
		consider(m)
	}
	for _, g := range c.Modules.MPIDependent {
		for _, m := range g.Modules {
			consider(m)
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].d != cs[j].d {
			return cs[i].d < cs[j].d
		}
		return cs[i].s < cs[j].s
	})
	var out []string
	for _, x := range cs {
		if len(out) == n || x.d > max(3, len(base)/2) {
			break
		}
		out = append(out, x.s)
	}
	return out
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func parseCatalog(b []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parsing cluster catalog: %w", err)
	}
	if !strings.HasPrefix(c.Schema, "ursa-catalog/") {
		return nil, fmt.Errorf("cluster catalog has unexpected schema %q", c.Schema)
	}
	return &c, nil
}
