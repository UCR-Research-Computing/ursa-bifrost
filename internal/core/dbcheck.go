package core

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Shared reference databases (v0.9.12): script_check points a script that downloads a
// database at the copy already in /data/shared, and warns when a large job array scans
// one of the big databases at once (the share's read throughput is cluster-wide).

// dbDownload is one way a script fetches a public database itself.
type dbDownload struct {
	re *regexp.Regexp
	// datasets this download duplicates, matched against catalog dataset names; the
	// warning is given only when one of them is actually hosted
	datasets []string
	what     string
}

var dbDownloads = []dbDownload{
	{regexp.MustCompile(`\bupdate_blastdb(\.pl)?\b|ftp\.ncbi\.nlm\.nih\.gov/blast/db\b|gs://blast-db\b`),
		[]string{"ncbi-blast"}, "NCBI BLAST databases"},
	{regexp.MustCompile(`ftp\.ncbi\.nlm\.nih\.gov/blast/db/FASTA/nr\.gz`),
		[]string{"diamond-nr", "ncbi-blast"}, "NCBI nr"},
	{regexp.MustCompile(`uniref90\.fasta\.gz`), []string{"uniref90"}, "UniRef90"},
	{regexp.MustCompile(`uniprot_sprot\.fasta\.gz`), []string{"swissprot"}, "Swiss-Prot"},
	{regexp.MustCompile(`\bkraken2-build\b[^\n]*--(download-library|standard)|genome-idx\.s3\.amazonaws\.com/kraken`),
		[]string{"kraken2-standard"}, "a Kraken2 database"},
	{regexp.MustCompile(`\bgtdbtk\s+download|download-db\.sh|gtdb[^\s]*/gtdbtk_r?\d+_data\.tar\.gz|gtdbtk_data\.tar\.gz`),
		[]string{"gtdbtk"}, "the GTDB-Tk reference data"},
	{regexp.MustCompile(`\bbakta_db\s+download\b`), []string{"bakta"}, "the Bakta database"},
	{regexp.MustCompile(`\bdownload_eggnog_data\.py\b`), []string{"eggnog"}, "the eggNOG-mapper data"},
	{regexp.MustCompile(`\bkaiju-makedb\b|kaiju-idx\.s3`), []string{"kaiju"}, "a Kaiju index"},
	{regexp.MustCompile(`\bcheckm2\s+database\s+--download\b`), []string{"checkm2"}, "the CheckM2 database"},
	{regexp.MustCompile(`\bcheckv\s+download_database\b`), []string{"checkv"}, "the CheckV database"},
	{regexp.MustCompile(`\bbusco\b[^\n]*--download(\s|$)|busco-data\.(ezlab\.org|s3)`), []string{"busco"}, "BUSCO lineages"},
	{regexp.MustCompile(`Pfam-A\.hmm\.gz`), []string{"pfam"}, "Pfam-A"},
	{regexp.MustCompile(`Rfam\.cm\.gz`), []string{"rfam"}, "Rfam"},
	{regexp.MustCompile(`\brun_dbcan\s+database\b`), []string{"dbcan"}, "the dbCAN database"},
	{regexp.MustCompile(`iprscan/5/[^\s]*interproscan-[^\s]*\.tar\.gz`), []string{"interproscan"}, "InterProScan"},
}

// big databases a scan reads end to end (hundreds of GB): many array tasks reading one
// at once share the share's ~100 MiB/s
var (
	reBigScan  = regexp.MustCompile(`\b(blastn|blastp|blastx|tblastn|tblastx)\b[^\n]*-db\s+['"]?(core_nt|nr|nt)\b|\bdiamond\s+(blastp|blastx)\b`)
	reArraySpc = regexp.MustCompile(`%(\d+)`)
)

// dbChecks adds script_check findings about shared reference databases.
func dbChecks(cat *Catalog, code, array string, add func(sev string, line int, msg string, a ...any)) {
	if cat == nil || len(cat.Datasets.Items) == 0 {
		return
	}
	hosted := map[string]Dataset{}
	for _, d := range cat.Datasets.Items {
		hosted[d.Name] = d
	}
	seen := map[string]bool{}
	for i, line := range strings.Split(code, "\n") {
		for _, dl := range dbDownloads {
			if !dl.re.MatchString(line) {
				continue
			}
			for _, name := range dl.datasets {
				d, ok := hosted[name]
				if !ok || seen[name] {
					continue
				}
				seen[name] = true
				env := ""
				if len(d.Env) > 0 {
					env = " (sets " + strings.Join(d.Env, ", ") + ")"
				}
				add("warning", 0, "line %d downloads %s, but the cluster already has a copy: `module load %s`%s points at %s (read-only). Downloading it again costs hours and %s of disk",
					i+1, dl.what, d.Module, env, d.Path, sizeText(d.SizeGB))
				break
			}
		}
	}
	if n := arrayConcurrency(array); n > 8 && reBigScan.MatchString(code) {
		mib := cat.Datasets.ReadMiBPerS
		if mib <= 0 {
			mib = 100
		}
		add("warning", 0, "this array runs up to %d tasks at once, each scanning a large database in %s; they share its ~%.0f MiB/s read throughput, so each gets ~%.0f MiB/s. Put many queries into each task (fewer, bigger tasks), or cap how many run at once (e.g. --array=1-100%%4)",
			n, cat.Datasets.Root, mib, mib/float64(n))
	}
}

// arrayConcurrency is how many tasks of an --array spec can run at once: the %N limit
// when given, else the number of indices (0 for no array).
func arrayConcurrency(spec string) int {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0
	}
	if m := reArraySpc.FindStringSubmatch(spec); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	total := 0
	for _, part := range strings.Split(spec, ",") {
		part, step, _ := strings.Cut(part, ":")
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(lo)
		if !isRange {
			if err1 == nil {
				total++
			}
			continue
		}
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || b < a {
			continue
		}
		s := 1
		if step != "" {
			if v, err := strconv.Atoi(step); err == nil && v > 0 {
				s = v
			}
		}
		total += (b-a)/s + 1
	}
	return total
}

func sizeText(gb float64) string {
	switch {
	case gb >= 1000:
		return fmt.Sprintf("%.1f TB", gb/1000)
	case gb >= 1:
		return fmt.Sprintf("%.0f GB", gb)
	case gb > 0:
		return fmt.Sprintf("%.1f GB", gb)
	}
	return "a lot"
}
