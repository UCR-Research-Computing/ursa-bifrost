package core

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
)

// Shared partitions (v0.8.0).
//
// Ursa Major is moving to shared nodes: every partition except highmem and gpul4
// will let several jobs share a node (Slurm OverSubscribe no longer EXCLUSIVE).
// On a shared partition a job gets only the cores and memory it asks for, and the
// cgroup holds it there; a job that asks for nothing gets one core. So on a shared
// partition bifrost:
//   - refuses a script that does not ask for cores (script_check error, which
//     blocks job_submit): --cpus-per-task, --ntasks(-per-node), or --exclusive;
//   - prices the share of the node the job holds, max(cores share, memory share),
//     instead of the whole node (worst case, caps, script_check estimate).
//
// Whether a partition is shared is read live from Slurm (`sinfo -h -o %R|%h`),
// cached like node state. Until the cluster changes, every partition reports
// EXCLUSIVE and nothing here changes behaviour. When the read fails, partitions
// count as exclusive: whole-node pricing is the safe (higher) estimate, and the
// core rule only fires on a partition known to be shared.

// partitionSharing maps partition -> shared (OverSubscribe is not EXCLUSIVE).
func (s *Service) partitionSharing(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	b, err := s.run(ctx, backend.PartitionSharing())
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, mode, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || name == "" {
			continue
		}
		mode = strings.ToUpper(strings.TrimSpace(mode))
		// EXCLUSIVE (whole nodes); NO, YES, YES:N, FORCE, FORCE:N all share nodes
		// between jobs under select/cons_tres (NO = no oversubscription of cores,
		// but different jobs may still use different cores of one node).
		out[strings.TrimSuffix(name, "*")] = mode != "" && mode != "EXCLUSIVE"
	}
	return out
}

// partitionShared reports whether one partition shares nodes now.
func (s *Service) partitionShared(ctx context.Context, part string) bool {
	return s.partitionSharing(ctx)[part]
}

// CoreRequest is what a batch script asks for per node.
type CoreRequest struct {
	Exclusive   bool    `json:"exclusive"`              // --exclusive: the whole node
	CoresAsked  bool    `json:"cores_asked"`            // any of -c/-n/--ntasks-per-node/--exclusive
	CoresPerNod int     `json:"cores_per_node"`         // cores the job holds per node
	MemMBPerNod int64   `json:"mem_mb_per_node"`        // memory per node (asked, or cores x DefMemPerCPU)
	MemAsked    bool    `json:"mem_asked"`              // --mem or --mem-per-cpu given
	NodeShare   float64 `json:"node_share"`             // share of each node the job is billed for
	Basis       string  `json:"basis,omitempty"`        // "cores" or "memory": which share decides
	DefaultNote string  `json:"default_note,omitempty"` // what Slurm gives when nothing is asked
}

// coresAsked reports whether the #SBATCH options ask for cores at all.
func coresAsked(req map[string]string) bool {
	for _, k := range []string{"cpus-per-task", "ntasks", "ntasks-per-node", "exclusive"} {
		if req[k] != "" {
			return true
		}
	}
	return false
}

// coreRequest works out cores, memory and the billed share of a node for a script
// on partition p. On an exclusive partition (shared false) the share is always 1.
func coreRequest(req map[string]string, p CatalogPartition, shared bool) CoreRequest {
	r := CoreRequest{Exclusive: req["exclusive"] != "" && req["exclusive"] != "false", CoresAsked: coresAsked(req)}
	nodeCores := p.CPUsPerNode
	nodeMemMB := int64(p.MemGBPerNode * 1024)
	cores := cpusPerNode(req) // 1 when nothing is asked, as Slurm does
	if nodeCores > 0 && cores > nodeCores {
		cores = nodeCores
	}
	if r.Exclusive || !shared {
		r.CoresPerNod, r.MemMBPerNod, r.NodeShare = nodeCores, nodeMemMB, 1
		return r
	}
	r.CoresPerNod = cores
	// memory: --mem (per node) wins, then --mem-per-cpu x cores, else the
	// partition default (DefMemPerCPU = node memory / node cores on Ursa Major)
	var mem int64
	if v, ok := memMB(req["mem"]); ok && v > 0 {
		mem, r.MemAsked = v, true
	} else if v, ok := memMB(req["mem-per-cpu"]); ok && v > 0 {
		mem, r.MemAsked = v*int64(cores), true
	} else if nodeCores > 0 {
		mem = nodeMemMB * int64(cores) / int64(nodeCores)
	}
	if nodeMemMB > 0 && mem > nodeMemMB {
		mem = nodeMemMB
	}
	r.MemMBPerNod = mem
	share, basis := 1.0, "cores"
	if nodeCores > 0 {
		share = float64(cores) / float64(nodeCores)
	}
	if nodeMemMB > 0 && mem > 0 {
		if m := float64(mem) / float64(nodeMemMB); m > share {
			share, basis = m, "memory"
		}
	}
	// cores and memory are capped above, so share is at most 1; no second cap
	r.NodeShare, r.Basis = share, basis
	if !r.CoresAsked {
		r.DefaultNote = fmt.Sprintf("asks for no cores, so Slurm gives it 1 core and about %.0f GB on %s", float64(mem)/1024, p.Name)
	}
	return r
}

// sharedSummary is one sentence on which partitions share nodes now.
func (s *Service) sharedSummary(ctx context.Context) string {
	sh := s.partitionSharing(ctx)
	var shared, whole []string
	for name, v := range sh {
		if v {
			shared = append(shared, name)
		} else {
			whole = append(whole, name)
		}
	}
	sort.Strings(shared)
	sort.Strings(whole)
	switch {
	case len(sh) == 0:
		return "Partitions are whole-node (exclusive) unless Slurm says otherwise; the sharing check could not be read."
	case len(shared) == 0:
		return "Partitions are whole-node (exclusive): a job gets and pays for whole nodes."
	default:
		msg := "Shared partitions (" + strings.Join(shared, ", ") + "): a job gets and pays for only the cores and memory it asks for, and must ask for cores (--cpus-per-task, --ntasks-per-node, or --exclusive for the whole node)."
		if len(whole) > 0 {
			msg += " Whole-node partitions (" + strings.Join(whole, ", ") + "): every job gets and pays for whole nodes."
		}
		return msg
	}
}

// allocShare is the share of each node a finished or running job held, from what
// Slurm allocated (accounting TRES or squeue): max(cores share, memory share), 1 when
// the node size is unknown. On exclusive partitions Slurm allocates the whole node,
// so this is 1 there without any sharing check; on shared partitions it is what the
// job actually held. Used for spent-cost figures (job lists, usage, waste).
func allocShare(cat *Catalog, part string, nodes int64, cpus, memMB int64) float64 {
	if cat == nil || nodes < 1 {
		return 1
	}
	p, ok := cat.Partition(part)
	if !ok || p.CPUsPerNode <= 0 {
		return 1
	}
	share := float64(cpus) / float64(nodes) / float64(p.CPUsPerNode)
	if nodeMem := p.MemGBPerNode * 1024; nodeMem > 0 && memMB > 0 {
		share = math.Max(share, float64(memMB)/float64(nodes)/nodeMem)
	}
	if share <= 0 || share > 1 {
		return 1
	}
	return share
}
