package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestShowCostTiers(t *testing.T) {
	ctx := context.Background()

	dollarFieldsPresent := func(t *testing.T, tool string, b []byte) bool {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("json unmarshal %s: %v", tool, err)
		}
		dataB, err := json.Marshal(m["data"])
		if err != nil {
			t.Fatalf("marshal data %s: %v", tool, err)
		}
		dataStr := string(dataB)
		// Check for dollar fields
		for _, field := range []string{
			`"est_cost_usd"`,
			`"usd_per_node_hour"`,
			`"current_usd_per_hour"`,
			`"est_wasted_usd"`,
			`"total_est_wasted_usd"`,
			`"usd_per_hour"`,
			`"worst_case_usd"`,
		} {
			if strings.Contains(dataStr, field) {
				return true
			}
		}
		return false
	}

	callTool := func(t *testing.T, s *Service, tool string) []byte {
		t.Helper()
		var res any
		var err error
		switch tool {
		case "cluster_status":
			res, err = Call(ctx, s, "test", tool, "R1", nil, false, s.ClusterStatus)
		case "partitions":
			res, err = Call(ctx, s, "test", tool, "R1", nil, false, s.Partitions)
		case "jobs_list":
			res, err = Call(ctx, s, "test", tool, "R1", nil, false, func(ctx context.Context) ([]JobSummary, error) {
				return s.JobsList(ctx, JobsListInput{Since: "now-30days"})
			})
		case "job_show":
			res, err = Call(ctx, s, "test", tool, "R1", map[string]any{"id": "236"}, false, func(ctx context.Context) (*JobDetail, error) {
				return s.JobShow(ctx, JobShowInput{JobID: "236"})
			})
		case "my_usage":
			res, err = Call(ctx, s, "test", tool, "R1", nil, false, func(ctx context.Context) (*Usage, error) {
				return s.Usage(ctx, UsageInput{Since: "now-30days"})
			})
		case "waste_report":
			res, err = Call(ctx, s, "test", tool, "R1", nil, false, func(ctx context.Context) (*WasteReport, error) {
				return s.Waste(ctx, WasteInput{Since: "now-30days"})
			})
		case "interactive_help":
			res, err = Call(ctx, s, "test", tool, "R1", nil, false, func(ctx context.Context) (*InteractiveHelp, error) {
				return s.InteractiveHelp(ctx, InteractiveInput{Partition: "computehigh", Time: "2:00:00", CPUs: 8})
			})
		default:
			t.Fatalf("unknown tool %s", tool)
		}
		if err != nil {
			t.Fatalf("call %s: %v", tool, err)
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal %s: %v", tool, err)
		}
		return b
	}

	tools := []string{
		"cluster_status",
		"partitions",
		"jobs_list",
		"job_show",
		"my_usage",
		"waste_report",
		"interactive_help",
	}

	tests := []struct {
		name          string
		callerTiers   []string
		showCostTiers []string
		showCost      bool
		ownCaps       bool
		wantDollars   bool
	}{
		{
			name:          "R1 caller with show_cost_tiers [R2] gets no dollar fields",
			callerTiers:   []string{"R1"},
			showCostTiers: []string{"R2"},
			showCost:      true,
			ownCaps:       false,
			wantDollars:   false,
		},
		{
			name:          "R2 caller with show_cost_tiers [R2] gets dollar fields",
			callerTiers:   []string{"R1", "R2"},
			showCostTiers: []string{"R2"},
			showCost:      true,
			ownCaps:       false,
			wantDollars:   true,
		},
		{
			name:          "unset show_cost_tiers keeps today output (all callers get dollars)",
			callerTiers:   []string{"R1"},
			showCostTiers: nil,
			showCost:      true,
			ownCaps:       false,
			wantDollars:   true,
		},
		{
			name:          "program client with own A1 caps always gets dollars",
			callerTiers:   []string{"R1"},
			showCostTiers: []string{"R2"},
			showCost:      true,
			ownCaps:       true,
			wantDollars:   true,
		},
		{
			name:          "show_cost false hides all dollars even for R2",
			callerTiers:   []string{"R1", "R2"},
			showCostTiers: []string{"R2"},
			showCost:      false,
			ownCaps:       false,
			wantDollars:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestService(t)
			s.Cfg.Tiers = tt.callerTiers
			s.Cfg.ShowCostTiers = tt.showCostTiers
			s.Cfg.ShowCost = tt.showCost
			s.ProgramOwnCaps = tt.ownCaps

			for _, tool := range tools {
				b := callTool(t, s, tool)
				gotDollars := dollarFieldsPresent(t, tool, b)
				if gotDollars != tt.wantDollars {
					t.Errorf("tool %s: gotDollars=%v, wantDollars=%v; json=%s", tool, gotDollars, tt.wantDollars, string(b))
				}
			}
		})
	}
}

func TestJobSubmitPlanKeepsDollarsForR1(t *testing.T) {
	s, _ := newTestService(t)
	s.Cfg.Tiers = []string{"R1", "A1"}
	s.Cfg.ShowCostTiers = []string{"R2"}
	s.Cfg.ShowCost = true

	script := "#!/bin/bash\n#SBATCH -p computehigh\n#SBATCH -N 1\n#SBATCH -t 01:00:00\n#SBATCH -c 8\nsrun hostname\n"
	res, err := Call(context.Background(), s, "test", "job_submit", "A1", nil, false, func(ctx context.Context) (*SubmitPlan, error) {
		return s.PrepareSubmit(ctx, SubmitInput{Script: script})
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Data.WorstCaseUSD <= 0 {
		t.Errorf("expected WorstCaseUSD in submit plan, got %v", res.Data.WorstCaseUSD)
	}
	if res.Data.DayCapUSD <= 0 {
		t.Errorf("expected DayCapUSD in submit plan, got %v", res.Data.DayCapUSD)
	}
}
