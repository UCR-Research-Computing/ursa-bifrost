package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/config"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/slurm"
)

func TestAllocationMath(t *testing.T) {
	ctx := context.Background()

	// 1. None: when allocation is nil, person and lab are null
	t.Run("none", func(t *testing.T) {
		s, _ := newTestService(t)
		s.Cfg.Allocation = nil
		res, err := Call(ctx, s, "test", "my_allocation", "R1", nil, false, s.MyAllocation)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Data
		if a.Person != nil {
			t.Errorf("expected Person to be nil, got %+v", a.Person)
		}
		if a.Lab != nil {
			t.Errorf("expected Lab to be nil, got %+v", a.Lab)
		}
		if a.Basis != "list-price estimate" || a.CoveredBy != "Research Computing / UCR ITS" || a.Enforcement != "warn" {
			t.Errorf("contract metadata mismatch: %+v", a)
		}
	})

	// 2. Person only
	t.Run("person only", func(t *testing.T) {
		s, _ := newTestService(t)
		s.Now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
		s.Cfg.Allocation = &config.AllocationConfig{
			PersonUSD: 500,
		}
		res, err := Call(ctx, s, "test", "my_allocation", "R1", nil, false, s.MyAllocation)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Data
		if a.Person == nil {
			t.Fatal("expected Person allocation, got nil")
		}
		if a.Lab != nil {
			t.Errorf("expected Lab to be nil, got %+v", a.Lab)
		}
		if a.Person.AllocationUSD != 500 {
			t.Errorf("expected AllocationUSD=500, got %v", a.Person.AllocationUSD)
		}
		if a.Person.UsedUSD <= 0 {
			t.Errorf("expected UsedUSD > 0, got %v", a.Person.UsedUSD)
		}
		if a.Person.LeftUSD != round(500-a.Person.UsedUSD, 2) {
			t.Errorf("left_usd = %v, want %v", a.Person.LeftUSD, round(500-a.Person.UsedUSD, 2))
		}
		if a.Person.Status != "ok" {
			t.Errorf("expected ok status, got %s", a.Person.Status)
		}
		if a.Period != "2026-09" || a.PeriodStart != "2026-09-01T00:00:00Z" || a.ResetsAt != "2026-10-01T00:00:00Z" {
			t.Errorf("unexpected period dates: %s, %s, %s", a.Period, a.PeriodStart, a.ResetsAt)
		}
	})

	// 3. Lab only
	t.Run("lab only", func(t *testing.T) {
		s, _ := newTestService(t)
		s.Now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
		s.Cfg.Allocation = &config.AllocationConfig{
			Lab: &config.AllocationLabConfig{
				ID:            "synthetic-lab",
				Name:          "Synthetic Lab",
				AllocationUSD: 1000,
				Period:        "month",
				Members:       []string{"alice_ucr_edu"},
			},
		}
		res, err := Call(ctx, s, "test", "my_allocation", "R1", nil, false, s.MyAllocation)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Data
		if a.Person != nil {
			t.Errorf("expected Person to be nil, got %+v", a.Person)
		}
		if a.Lab == nil {
			t.Fatal("expected Lab allocation, got nil")
		}
		if a.Lab.ID != "synthetic-lab" || a.Lab.Name != "Synthetic Lab" || a.Lab.AllocationUSD != 1000 {
			t.Errorf("unexpected lab info: %+v", a.Lab)
		}
		if a.Lab.UsedUSD <= 0 {
			t.Errorf("expected lab UsedUSD > 0, got %v", a.Lab.UsedUSD)
		}
		if a.Lab.Members != 1 {
			t.Errorf("expected 1 member, got %d", a.Lab.Members)
		}
	})

	// 4. Both configured
	t.Run("both", func(t *testing.T) {
		s, _ := newTestService(t)
		s.Now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
		s.Cfg.Allocation = &config.AllocationConfig{
			PersonUSD: 500,
			Lab: &config.AllocationLabConfig{
				ID:            "synthetic-lab",
				Name:          "Synthetic Lab",
				AllocationUSD: 1000,
				Period:        "month",
				Members:       []string{"alice_ucr_edu"},
			},
		}
		res, err := Call(ctx, s, "test", "my_allocation", "R1", nil, false, s.MyAllocation)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Data
		if a.Person == nil || a.Lab == nil {
			t.Fatalf("expected both person and lab, got person=%v lab=%v", a.Person, a.Lab)
		}
		// Verify exact JSON contract keys
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"period", "period_start", "resets_at", "basis", "covered_by", "person", "lab", "enforcement", "notes"} {
			if _, ok := m[key]; !ok {
				t.Errorf("missing contract key %q in JSON: %s", key, string(b))
			}
		}
	})

	// 5. Over 100% status and non-negative left_usd
	t.Run("over 100 percent", func(t *testing.T) {
		s, _ := newTestService(t)
		s.Now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
		// Set allocation very low so spent exceeds it
		s.Cfg.Allocation = &config.AllocationConfig{
			PersonUSD: 1.0,
			Lab: &config.AllocationLabConfig{
				ID:            "synthetic-lab",
				Name:          "Synthetic Lab",
				AllocationUSD: 1.0,
				Period:        "month",
				Members:       []string{"alice_ucr_edu"},
			},
		}
		res, err := Call(ctx, s, "test", "my_allocation", "R1", nil, false, s.MyAllocation)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Data
		if a.Person.Status != "over" || a.Person.PctUsed < 100 {
			t.Errorf("person: expected over status and >=100%%, got %+v", a.Person)
		}
		if a.Person.LeftUSD != 0 {
			t.Errorf("person left_usd must be 0 when over, got %v", a.Person.LeftUSD)
		}
		if a.Lab.Status != "over" || a.Lab.PctUsed < 100 {
			t.Errorf("lab: expected over status and >=100%%, got %+v", a.Lab)
		}
		if a.Lab.LeftUSD != 0 {
			t.Errorf("lab left_usd must be 0 when over, got %v", a.Lab.LeftUSD)
		}
	})

	// 6. Warn status (80 to 99.9%)
	t.Run("warn status", func(t *testing.T) {
		s, _ := newTestService(t)
		s.Now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
		// alice used $43.24 in September fixture jobs.
		// If allocation is 50, used/alloc is 43.24/50 = 86.5%
		s.Cfg.Allocation = &config.AllocationConfig{
			PersonUSD: 50,
		}
		res, err := Call(ctx, s, "test", "my_allocation", "R1", nil, false, s.MyAllocation)
		if err != nil {
			t.Fatal(err)
		}
		a := res.Data
		if a.Person.Status != "warn" || a.Person.PctUsed < 80 || a.Person.PctUsed >= 100 {
			t.Errorf("expected warn status and 80 <= pct < 100, got %+v", a.Person)
		}
	})

	// 6. Period boundary at month start UTC
	t.Run("period boundary", func(t *testing.T) {
		now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		period, start, resets := periodBounds(now)
		if period != "2026-10" {
			t.Errorf("period = %s, want 2026-10", period)
		}
		if start.Format(time.RFC3339) != "2026-10-01T00:00:00Z" {
			t.Errorf("start = %s", start.Format(time.RFC3339))
		}
		if resets.Format(time.RFC3339) != "2026-11-01T00:00:00Z" {
			t.Errorf("resets = %s", resets.Format(time.RFC3339))
		}

		// A job ending at 2026-09-30T23:59:59Z is before start
		var preJob slurm.AcctJob
		preJob.Time.End = start.Add(-time.Second).Unix()
		if !jobTimestamp(preJob).Before(start) {
			t.Errorf("preJob should be before start")
		}

		// A job ending at 2026-10-01T00:00:00Z is in range
		var curJob slurm.AcctJob
		curJob.Time.End = start.Unix()
		jt := jobTimestamp(curJob)
		if jt.Before(start) || !jt.Before(resets) {
			t.Errorf("curJob should be within [start, resets)")
		}

		// A job at 2026-11-01T00:00:00Z is on/after resets
		var postJob slurm.AcctJob
		postJob.Time.End = resets.Unix()
		if jobTimestamp(postJob).Before(resets) {
			t.Errorf("postJob should not be before resets")
		}
	})
}

func TestLabTotalExcludesNonMembersAndNeverExposesPerMember(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	s.Now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
	s.Cfg.Tiers = []string{"R1"}

	// Configure lab with alice and bob
	s.Cfg.Allocation = &config.AllocationConfig{
		PersonUSD: 500,
		Lab: &config.AllocationLabConfig{
			ID:            "synthetic-lab",
			Name:          "Synthetic Lab",
			AllocationUSD: 1000,
			Period:        "month",
			Members:       []string{"alice_ucr_edu", "bob_ucr_edu"},
		},
	}

	res, err := Call(ctx, s, "test", "my_allocation", "R1", nil, false, s.MyAllocation)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Check lab members count
	if res.Data.Lab.Members != 2 {
		t.Errorf("expected 2 members, got %d", res.Data.Lab.Members)
	}

	// 2. Check that R1 never sees per-member rows
	if len(res.Data.Notes) != 0 {
		t.Errorf("R1 notes must not leak member details, got %v", res.Data.Notes)
	}

	// Check JSON output does not contain any member username other than own
	b, err := json.Marshal(res.Data)
	if err != nil {
		t.Fatal(err)
	}
	jsonStr := string(b)
	if strings.Contains(jsonStr, "bob_ucr_edu") {
		t.Errorf("JSON leaked other member username: %s", jsonStr)
	}

	// 3. For R2 caller, staff notes contain member info
	s.Cfg.Tiers = []string{"R1", "R2"}
	resR2, err := Call(ctx, s, "test", "my_allocation", "R1", nil, false, s.MyAllocation)
	if err != nil {
		t.Fatal(err)
	}
	if len(resR2.Data.Notes) == 0 || !strings.Contains(resR2.Data.Notes[0], "bob_ucr_edu") {
		t.Errorf("R2 caller expected staff member note, got %v", resR2.Data.Notes)
	}
}

func TestJobSubmitPlanAllocationWarnings(t *testing.T) {
	ctx := context.Background()

	// 1. Under 80%: no warning, confirm token returned
	t.Run("under 80 percent", func(t *testing.T) {
		s, _ := newTestService(t)
		s.Now = func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) }
		s.Cfg.Tiers = []string{"R1", "A1"}
		s.Cfg.Allocation = &config.AllocationConfig{
			PersonUSD: 10000,
			Lab: &config.AllocationLabConfig{
				ID:            "synthetic-lab",
				Name:          "Synthetic Lab",
				AllocationUSD: 20000,
				Period:        "month",
				Members:       []string{"alice_ucr_edu"},
			},
		}

		script := "#!/bin/bash\n#SBATCH -p computehigh\n#SBATCH -N 1\n#SBATCH -t 00:30:00\n#SBATCH -c 4\nsrun hostname\n"
		res, err := Call(ctx, s, "test", "job_submit", "A1", nil, false, func(ctx context.Context) (*SubmitPlan, error) {
			return s.PrepareSubmit(ctx, SubmitInput{Script: script})
		})
		if err != nil {
			t.Fatal(err)
		}
		plan := res.Data
		if plan.Token == "" {
			t.Fatal("confirm_token missing")
		}
		if plan.Allocation == nil {
			t.Fatal("allocation info missing in plan")
		}
		if plan.Allocation.Warning != "" {
			t.Errorf("expected no allocation warning at <80%%, got: %s", plan.Allocation.Warning)
		}
		if plan.Allocation.PersonPctAfter == nil || *plan.Allocation.PersonPctAfter >= 80 {
			t.Errorf("expected PersonPctAfter < 80, got %v", plan.Allocation.PersonPctAfter)
		}
	})

	// 2. 80-99.9%: warning returned, confirm token still returned
	t.Run("warn 80 to 99 percent", func(t *testing.T) {
		s, _ := newTestService(t)
		s.Now = func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) }
		s.Cfg.Tiers = []string{"R1", "A1"}
		// 1 h on computehigh is 1.87 * 1 = 1.87 USD worst case
		// If PersonUSD = 2.2, 1.87 / 2.2 = 85.0%
		s.Cfg.Allocation = &config.AllocationConfig{
			PersonUSD: 2.2,
		}

		script := "#!/bin/bash\n#SBATCH -p computehigh\n#SBATCH -N 1\n#SBATCH -t 01:00:00\n#SBATCH -c 22\nsrun hostname\n"
		res, err := Call(ctx, s, "test", "job_submit", "A1", nil, false, func(ctx context.Context) (*SubmitPlan, error) {
			return s.PrepareSubmit(ctx, SubmitInput{Script: script})
		})
		if err != nil {
			t.Fatal(err)
		}
		plan := res.Data
		if plan.Token == "" {
			t.Fatal("confirm_token missing (must never block)")
		}
		if plan.Allocation == nil || plan.Allocation.Warning == "" {
			t.Fatalf("expected allocation warning at 80-99%%, got %+v", plan.Allocation)
		}
		if !strings.Contains(plan.Allocation.Warning, "warn-only for now; it will still run") {
			t.Errorf("expected warn-only text in warning: %s", plan.Allocation.Warning)
		}
		if plan.Allocation.PersonPctAfter == nil || *plan.Allocation.PersonPctAfter < 80 || *plan.Allocation.PersonPctAfter >= 100 {
			t.Errorf("expected 80 <= PersonPctAfter < 100, got %v", plan.Allocation.PersonPctAfter)
		}
	})

	// 3. >= 100%: over warning returned, confirm token STILL returned (never blocks)
	t.Run("over 100 percent", func(t *testing.T) {
		s, _ := newTestService(t)
		s.Now = func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) }
		s.Cfg.Tiers = []string{"R1", "A1"}
		// 1 h on computehigh is 1.87 worst case.
		// If Lab allocation is 1.0, worst case pushes to 187%
		s.Cfg.Allocation = &config.AllocationConfig{
			Lab: &config.AllocationLabConfig{
				ID:            "synthetic-lab",
				Name:          "Synthetic Lab",
				AllocationUSD: 1.0,
				Period:        "month",
				Members:       []string{"alice_ucr_edu"},
			},
		}

		script := "#!/bin/bash\n#SBATCH -p computehigh\n#SBATCH -N 1\n#SBATCH -t 01:00:00\n#SBATCH -c 22\nsrun hostname\n"
		res, err := Call(ctx, s, "test", "job_submit", "A1", nil, false, func(ctx context.Context) (*SubmitPlan, error) {
			return s.PrepareSubmit(ctx, SubmitInput{Script: script})
		})
		if err != nil {
			t.Fatal(err)
		}
		plan := res.Data
		if plan.Token == "" {
			t.Fatal("confirm_token missing: allocation over 100% must NEVER block")
		}
		if plan.Allocation == nil || plan.Allocation.Warning == "" {
			t.Fatalf("expected allocation warning at >=100%%, got %+v", plan.Allocation)
		}
		if !strings.Contains(plan.Allocation.Warning, "take your lab to") || !strings.Contains(plan.Allocation.Warning, "warn-only for now; it will still run") {
			t.Errorf("expected lab over warning, got: %s", plan.Allocation.Warning)
		}
		if plan.Allocation.LabPctAfter == nil || *plan.Allocation.LabPctAfter < 100 {
			t.Errorf("expected LabPctAfter >= 100, got %v", plan.Allocation.LabPctAfter)
		}
	})
}
