package backend

import (
	"encoding/json"
	"fmt"
	"strings"
)

// sacctRowsFromJSON prints a recorded `sacct --json` answer the way
// `sacct -n -P --noconvert -o <SacctFields>` with SLURM_TIME_FORMAT=%s prints
// the same jobs on Slurm 25.11 (checked field by field against 745 live jobs,
// SPEC section 23): a job row, then one row per step.
func sacctRowsFromJSON(b []byte) ([]byte, error) {
	type num struct {
		Set    bool    `json:"set"`
		Number float64 `json:"number"`
	}
	type exit struct {
		ReturnCode num `json:"return_code"`
		Signal     struct {
			ID num `json:"id"`
		} `json:"signal"`
	}
	type tres struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Count int64  `json:"count"`
	}
	type cpu struct {
		Seconds      int64 `json:"seconds"`
		Microseconds int64 `json:"microseconds"`
	}
	var doc struct {
		Jobs []struct {
			JobID     int64  `json:"job_id"`
			Name      string `json:"name"`
			User      string `json:"user"`
			Partition string `json:"partition"`
			State     struct {
				Current []string `json:"current"`
				Reason  string   `json:"reason"`
			} `json:"state"`
			ExitCode        exit   `json:"exit_code"`
			Nodes           string `json:"nodes"`
			AllocationNodes int64  `json:"allocation_nodes"`
			RestartCnt      int64  `json:"restart_cnt"`
			Time            struct {
				Elapsed    int64 `json:"elapsed"`
				Start      int64 `json:"start"`
				End        int64 `json:"end"`
				Submission int64 `json:"submission"`
				Total      cpu   `json:"total"`
			} `json:"time"`
			TRES struct {
				Allocated []tres `json:"allocated"`
			} `json:"tres"`
			Steps []struct {
				Step struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"step"`
				State    []string `json:"state"`
				ExitCode exit     `json:"exit_code"`
				Time     struct {
					Elapsed int64 `json:"elapsed"`
					Start   num   `json:"start"` // steps wrap their times
					End     num   `json:"end"`
					Total   cpu   `json:"total"`
				} `json:"time"`
				TRES struct {
					Requested struct {
						Max []tres `json:"max"`
					} `json:"requested"`
				} `json:"tres"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	ec := func(e exit) string {
		if e.Signal.ID.Set {
			return fmt.Sprintf("0:%d", int64(e.Signal.ID.Number))
		}
		return fmt.Sprintf("%d:0", int64(e.ReturnCode.Number))
	}
	ts := func(v int64) string {
		if v <= 0 || v >= 4294967294 {
			return "None"
		}
		return fmt.Sprint(v)
	}
	cp := func(c cpu) string {
		s := float64(c.Seconds) + float64(c.Microseconds)/1e6
		h, m := int64(s)/3600, (int64(s)%3600)/60
		sec := s - float64(h*3600+m*60)
		if h > 0 {
			return fmt.Sprintf("%02d:%02d:%02d", h, m, int64(sec))
		}
		return fmt.Sprintf("%02d:%06.3f", m, sec)
	}
	var out strings.Builder
	for _, j := range doc.Jobs {
		var tr []string
		for _, t := range j.TRES.Allocated {
			if t.Type == "energy" {
				continue // --json lists it, the text form does not
			}
			k := t.Type
			if t.Name != "" {
				k += "/" + t.Name
			}
			v := fmt.Sprint(t.Count)
			if t.Type == "mem" {
				v += "M"
			}
			tr = append(tr, k+"="+v)
		}
		nodes, nn := j.Nodes, j.AllocationNodes
		if nn == 0 {
			nodes, nn = "None assigned", 1
		}
		fmt.Fprintf(&out, "%d|%s|%s|%s|%s|%s|%s|%d|%s|%s|%s|%d|%s|%s||%d|%s\n",
			j.JobID, j.User, j.Partition, strings.Join(j.State.Current, ","), j.State.Reason, ec(j.ExitCode),
			nodes, nn, ts(j.Time.Submission), ts(j.Time.Start), ts(j.Time.End), j.Time.Elapsed,
			cp(j.Time.Total), strings.Join(tr, ","), j.RestartCnt, j.Name)
		for _, s := range j.Steps {
			rss := ""
			for _, t := range s.TRES.Requested.Max {
				if t.Type == "mem" {
					rss = fmt.Sprint(t.Count)
				}
			}
			fmt.Fprintf(&out, "%s|||%s||%s|%s|1|%s|%s|%s|%d|%s||%s||%s\n",
				s.Step.ID, strings.Join(s.State, ","), ec(s.ExitCode), j.Nodes, ts(int64(s.Time.Start.Number)),
				ts(int64(s.Time.Start.Number)), ts(int64(s.Time.End.Number)), s.Time.Elapsed, cp(s.Time.Total), rss, s.Step.Name)
		}
	}
	return []byte(out.String()), nil
}
