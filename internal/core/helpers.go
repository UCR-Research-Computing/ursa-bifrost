package core

// Helper tools that stand in for a shell (SPEC section 18.4): storage_usage,
// env_check, interactive_help. Each runs fixed, validated commands as the
// person, or nothing at all.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
)

// ---- storage_usage ---------------------------------------------------------------

// FSUsage is one filesystem as `df` sees it.
type FSUsage struct {
	Mount      string  `json:"mount"`
	SizeBytes  int64   `json:"size_bytes"`
	UsedBytes  int64   `json:"used_bytes"`
	FreeBytes  int64   `json:"free_bytes"`
	UsedPct    float64 `json:"used_pct"`
	SharedNote string  `json:"note"`
}

// FolderUsage is one top-level entry of the person's folder.
type FolderUsage struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	// Partial: du ran out of its time budget for this entry (size unknown).
	Partial bool `json:"unknown,omitempty"`
}

// StorageUsage is storage_usage's answer.
type StorageUsage struct {
	User        string        `json:"user"`
	Filesystems []FSUsage     `json:"filesystems"`
	Folders     []FolderUsage `json:"largest"` // biggest first, top 25
	HomeBytes   int64         `json:"home_bytes"`
	ScratchByte int64         `json:"scratch_bytes"`
	Notes       []string      `json:"notes"`
}

// StorageUsage reports disk use in the person's home and scratch folders.
func (s *Service) StorageUsage(ctx context.Context) (*StorageUsage, error) {
	user, roots, err := s.userRoots(ctx)
	if err != nil {
		return nil, err
	}
	r := &StorageUsage{User: user, Filesystems: []FSUsage{}, Folders: []FolderUsage{}, Notes: []string{}}
	dc, err := backend.DiskFree(roots...)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, dc)
	if err != nil {
		return nil, fmt.Errorf("df: %w", err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[0] == "Filesystem" || seen[f[5]] {
			continue
		}
		seen[f[5]] = true
		size, _ := strconv.ParseInt(f[1], 10, 64)
		used, _ := strconv.ParseInt(f[2], 10, 64)
		free, _ := strconv.ParseInt(f[3], 10, 64)
		pct := 0.0
		if size > 0 {
			pct = round(100*float64(used)/float64(size), 1)
		}
		r.Filesystems = append(r.Filesystems, FSUsage{Mount: f[5], SizeBytes: size, UsedBytes: used, FreeBytes: free, UsedPct: pct,
			SharedNote: "shared by every user of the cluster"})
	}
	uc, err := backend.DuTop(roots...)
	if err != nil {
		return nil, err
	}
	out, err = s.run(ctx, uc)
	if err != nil {
		return nil, fmt.Errorf("du: %w", err)
	}
	partial := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		size, p, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if sensitive(path.Base(p)) {
			continue
		}
		fu := FolderUsage{Path: p}
		if size == "?" {
			fu.Partial = true
			partial++
		} else {
			fu.Bytes, _ = strconv.ParseInt(size, 10, 64)
		}
		switch {
		case strings.HasPrefix(p, roots[0]+"/"):
			r.HomeBytes += fu.Bytes
		case strings.HasPrefix(p, roots[1]+"/"):
			r.ScratchByte += fu.Bytes
		}
		r.Folders = append(r.Folders, fu)
	}
	sort.Slice(r.Folders, func(i, j int) bool { return r.Folders[i].Bytes > r.Folders[j].Bytes })
	if len(r.Folders) > 25 {
		r.Folders = r.Folders[:25]
	}
	if partial > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d folder(s) were too big to size within the time limit (shown as unknown); totals are a lower bound.", partial))
		markTruncated(ctx)
	}
	r.Notes = append(r.Notes, "No per-user quota is set on this cluster; the filesystems are shared. Hidden folders such as .cache and .conda are included because they often fill a home folder.")
	markUntrusted(ctx) // folder names are written by people and programs
	return r, nil
}

// ---- env_check -------------------------------------------------------------------

// versionCommands are the programs env_check asks for --version (fixed list:
// running an arbitrary program, even with --version, is not allowed).
var versionCommands = map[string]bool{
	"python3": true, "python": true, "pip3": true, "gcc": true, "g++": true, "gfortran": true,
	"mpirun": true, "mpicc": true, "nvcc": true, "cmake": true, "make": true, "R": true, "julia": true,
	"java": true, "apptainer": true, "singularity": true, "conda": true, "pixi": true, "go": true,
	"rustc": true, "cargo": true, "node": true, "git": true, "perl": true,
}

// ModuleLoad is one module load attempt.
type ModuleLoad struct {
	Module string `json:"module"`
	Loaded bool   `json:"loaded"`
	Error  string `json:"error,omitempty"` // Lmod's message (redacted)
}

// CommandInfo is where a command resolves after the modules are loaded.
type CommandInfo struct {
	Command string `json:"command"`
	Path    string `json:"path,omitempty"` // empty = not found
	Version string `json:"version,omitempty"`
}

// EnvCheck is env_check's answer.
type EnvCheck struct {
	Modules  []ModuleLoad  `json:"modules"`
	Loaded   []string      `json:"module_list"`
	Commands []CommandInfo `json:"commands"`
	Notes    []string      `json:"notes"`
}

// EnvCheck loads modules on the login node and reports what you get.
func (s *Service) EnvCheck(ctx context.Context, modules, commands []string) (*EnvCheck, error) {
	if len(modules) == 0 && len(commands) == 0 {
		commands = []string{"python3", "gcc", "mpirun"}
	}
	c, err := backend.EnvCheck(modules, commands, versionCommands)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("env_check: %w", err)
	}
	r := &EnvCheck{Modules: []ModuleLoad{}, Loaded: []string{}, Commands: []CommandInfo{}, Notes: []string{}}
	inList := false
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		switch {
		case f[0] == "M" && len(f) >= 3:
			ml := ModuleLoad{Module: f[1], Loaded: f[2] == "ok"}
			if !ml.Loaded && len(f) >= 4 {
				ml.Error = policy.CleanLabel(f[3], 300)
			}
			r.Modules = append(r.Modules, ml)
		case f[0] == "L" && len(f) == 1:
			inList = true
		case f[0] == "C" && len(f) >= 3:
			inList = false
			ci := CommandInfo{Command: f[1], Path: f[2]}
			if len(f) >= 4 {
				ci.Version = policy.CleanLabel(f[3], 200)
			}
			r.Commands = append(r.Commands, ci)
		case inList:
			r.Loaded = append(r.Loaded, policy.CleanLabel(strings.TrimSpace(line), 120))
		}
	}
	for _, ci := range r.Commands {
		if ci.Path == "" {
			switch ci.Command {
			case "python":
				r.Notes = append(r.Notes, "There is no bare `python` on Ursa Major; use python3 (or a conda/pixi environment).")
			case "nvcc":
				r.Notes = append(r.Notes, "nvcc comes from a cuda module and is only useful on the gpul4 partition.")
			case "mpirun", "mpicc":
				r.Notes = append(r.Notes, "MPI tools appear after `module load openmpi` (or mpich / intel-oneapi-mpi).")
			}
		}
	}
	r.Notes = append(r.Notes, "Checked on the login node; compute nodes share /apps, so modules resolve the same in batch jobs.")
	return r, nil
}

// ---- interactive_help ------------------------------------------------------------

// InteractiveInput is what the person wants an interactive session for.
type InteractiveInput struct {
	Partition string
	Nodes     int
	CPUs      int
	GPUs      int
	Time      string
	Memory    string
}

// InteractiveHelp is interactive_help's answer. It runs nothing.
type InteractiveHelp struct {
	Partition    string   `json:"partition"`
	TimeLimit    string   `json:"time_limit"`
	Connect      string   `json:"connect"`        // how to reach the login node
	Command      string   `json:"command"`        // one-step: shell on a compute node
	Alternative  string   `json:"alternative"`    // salloc, then srun
	USDPerHour   float64  `json:"usd_per_hour"`   // whole nodes, or the share held on shared partitions
	WorstCaseUSD float64  `json:"worst_case_usd"` // if the session runs to the limit
	Warnings     []string `json:"warnings"`
	Notes        []string `json:"notes"`
}

// InteractiveHelp writes the commands for an interactive session.
func (s *Service) InteractiveHelp(ctx context.Context, in InteractiveInput) (*InteractiveHelp, error) {
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	part := firstNonEmpty(in.Partition, cat.DefaultPartition())
	cp, ok := cat.Partition(part)
	if !ok {
		names := []string{}
		for _, p := range cat.Partitions {
			names = append(names, p.Name)
		}
		return nil, fmt.Errorf("partition %q does not exist (choose one of %s)", part, strings.Join(names, ", "))
	}
	mins, ok := slurmMinutes(firstNonEmpty(in.Time, "60"))
	if !ok || mins <= 0 {
		return nil, errors.New("time must be a Slurm time limit like 60, 2:00:00 or 1-00:00")
	}
	nodes := max(in.Nodes, 1)
	if nodes > cp.MaxNodes && cp.MaxNodes > 0 {
		return nil, fmt.Errorf("%d nodes requested; partition %s has %d", nodes, part, cp.MaxNodes)
	}
	if in.CPUs < 0 || (cp.CPUsPerNode > 0 && in.CPUs > cp.CPUsPerNode) {
		return nil, fmt.Errorf("cpus must be 1-%d on %s", cp.CPUsPerNode, part)
	}
	if in.GPUs < 0 || in.GPUs > cp.GPUsPerNode {
		if cp.GPUsPerNode == 0 {
			return nil, fmt.Errorf("partition %s has no GPUs; use gpul4", part)
		}
		return nil, fmt.Errorf("gpus must be 0-%d on %s", cp.GPUsPerNode, part)
	}
	if in.Memory != "" && !reMemory.MatchString(in.Memory) {
		return nil, errors.New("memory must look like 16G or 4000M")
	}
	r := &InteractiveHelp{Partition: part, TimeLimit: minutesText(mins), Warnings: []string{}, Notes: []string{}}
	args := []string{"-p " + part, fmt.Sprintf("-N %d", nodes), "-t " + slurmTime(mins)}
	if in.CPUs > 0 {
		args = append(args, fmt.Sprintf("-c %d", in.CPUs))
	}
	if in.GPUs > 0 {
		args = append(args, fmt.Sprintf("--gres=gpu:%d", in.GPUs))
	}
	if in.Memory != "" {
		args = append(args, "--mem="+in.Memory)
	}
	a := strings.Join(args, " ")
	r.Command = "srun " + a + " --pty bash -l"
	r.Alternative = "salloc " + a + "   # then: srun --pty bash -l   (exit twice to release the node)"
	ic := s.Cfg.IAP
	inst, zone, proj := firstNonEmpty(ic.Instance, "ucrslurmcl-slurm-login-001"), firstNonEmpty(ic.Zone, "us-central1-a"), firstNonEmpty(ic.Project, "ucr-ursa-major-hpc-cluster")
	r.Connect = fmt.Sprintf("gcloud compute ssh %s --zone %s --project %s --tunnel-through-iap", inst, zone, proj)
	// shared partitions bill the share of the node the session holds (v0.8.0)
	shared := s.partitionShared(ctx, part)
	share := 1.0
	if shared {
		req := map[string]string{}
		if in.CPUs > 0 {
			req["cpus-per-task"] = strconv.Itoa(in.CPUs)
		}
		if in.Memory != "" {
			req["mem"] = in.Memory
		}
		cr := coreRequest(req, cp, true)
		share = cr.NodeShare
		if in.CPUs == 0 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s shares nodes between jobs: without cpus the session gets 1 core. Set cpus to what you need.", part))
		}
	}
	if price, priced := s.price(cat, part); priced {
		r.USDPerHour = round(price*float64(nodes)*share, 2)
		r.WorstCaseUSD = round(price*float64(nodes)*share*float64(mins)/60, 2)
	}
	if cp.GPUsPerNode > 0 && in.GPUs == 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s nodes have a GPU, but without --gres=gpu:1 your session cannot see it.", part))
	}
	if cp.Spot {
		r.Warnings = append(r.Warnings, "Spot nodes can be taken back by Google at any time; do not use them for work you cannot lose.")
	}
	if float64(mins) > s.Cfg.Caps.MaxHours*60 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("That is longer than the %.0f h bifrost cap for batch jobs; an idle interactive session still bills what it holds.", s.Cfg.Caps.MaxHours))
	}
	r.Notes = append(r.Notes,
		"bifrost does not open shells. Run these yourself: first connect to the login node, then start the session.",
		"Nodes power up on demand, so the first session can take a few minutes to start.",
		"The session is billed until it ends: exit as soon as you are done.",
		"Do not run heavy work on the login node itself; that is what the interactive session is for.")
	if !s.Cfg.ShowCost {
		r.USDPerHour, r.WorstCaseUSD = 0, 0
	}
	return r, nil
}

var reMemory = regexp.MustCompile(`^[1-9][0-9]{0,6}[KMGT]?$`)

func slurmTime(mins int) string {
	if mins >= 24*60 {
		return fmt.Sprintf("%d-%02d:%02d:00", mins/(24*60), (mins%(24*60))/60, mins%60)
	}
	return fmt.Sprintf("%d:%02d:00", mins/60, mins%60)
}
