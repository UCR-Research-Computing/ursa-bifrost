package core

import (
	"context"
	"fmt"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/slurm"
)

// PersonAllocation is one researcher's allocation status.
type PersonAllocation struct {
	AllocationUSD float64 `json:"allocation_usd"`
	UsedUSD       float64 `json:"used_usd"`
	LeftUSD       float64 `json:"left_usd"`
	PctUsed       float64 `json:"pct_used"`
	Status        string  `json:"status"` // ok | warn | over
}

// LabAllocation is a research group's allocation status.
type LabAllocation struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	AllocationUSD float64 `json:"allocation_usd"`
	UsedUSD       float64 `json:"used_usd"`
	LeftUSD       float64 `json:"left_usd"`
	PctUsed       float64 `json:"pct_used"`
	Members       int     `json:"members"`
	Status        string  `json:"status"` // ok | warn | over
}

// MyAllocation is the my_allocation answer.
type MyAllocation struct {
	Period      string            `json:"period"`
	PeriodStart string            `json:"period_start"`
	ResetsAt    string            `json:"resets_at"`
	Basis       string            `json:"basis"`
	CoveredBy   string            `json:"covered_by"`
	Person      *PersonAllocation `json:"person"`
	Lab         *LabAllocation    `json:"lab"`
	Enforcement string            `json:"enforcement"`
	Notes       []string          `json:"notes"`
}

// PlanAllocation is the allocation impact in a job_submit plan.
type PlanAllocation struct {
	PersonPctAfter *float64 `json:"person_pct_after"`
	LabPctAfter    *float64 `json:"lab_pct_after"`
	Warning        string   `json:"warning,omitempty"`
}

func periodBounds(now time.Time) (period string, start, resets time.Time) {
	utc := now.UTC()
	period = fmt.Sprintf("%04d-%02d", utc.Year(), utc.Month())
	start = time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	resets = start.AddDate(0, 1, 0)
	return
}

func jobTimestamp(j slurm.AcctJob) time.Time {
	for _, t := range []int64{j.Time.End, j.Time.Start, j.Time.Submission} {
		if t > 0 {
			return time.Unix(t, 0).UTC()
		}
	}
	return time.Time{}
}

func calcAllocation(allocUSD, usedUSD float64) (used, left, pct float64, status string) {
	used = round(usedUSD, 2)
	alloc := round(allocUSD, 2)
	left = round(alloc-used, 2)
	if left < 0 {
		left = 0
	}
	if alloc > 0 {
		pct = round((used/alloc)*100, 1)
	}
	switch {
	case pct >= 100.0:
		status = "over"
	case pct >= 80.0:
		status = "warn"
	default:
		status = "ok"
	}
	return
}

func hasTier(tiers []string, want string) bool {
	for _, t := range tiers {
		if t == want {
			return true
		}
	}
	return false
}

// allocationUsage computes list-price spend for the current UTC month for the caller
// and for their lab members (if a lab is configured).
func (s *Service) allocationUsage(ctx context.Context) (personUsed, labUsed float64, labMembers []string, memberSpend map[string]float64, err error) {
	if s.Cfg.Allocation == nil {
		return 0, 0, nil, nil, nil
	}
	me, err := s.User(ctx)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	now := s.Now()
	_, start, resets := periodBounds(now)
	since := start.Format("2006-01-02T15:04:05")

	hasPerson := s.Cfg.Allocation.PersonUSD > 0
	hasLab := s.Cfg.Allocation.Lab != nil && s.Cfg.Allocation.Lab.AllocationUSD > 0

	if !hasPerson && !hasLab {
		return 0, 0, nil, nil, nil
	}

	var users []string
	if hasLab {
		users = append([]string(nil), s.Cfg.Allocation.Lab.Members...)
		if len(users) == 0 {
			users = []string{me}
		}
		foundMe := false
		for _, u := range users {
			if u == me {
				foundMe = true
				break
			}
		}
		if !foundMe {
			users = append(users, me)
		}
		labMembers = users
	} else {
		users = []string{me}
	}

	isMember := map[string]bool{}
	for _, u := range labMembers {
		isMember[u] = true
	}

	c, err := backend.SacctSummaryUsers(users, since, "")
	if err != nil {
		return 0, 0, nil, nil, err
	}
	jobs, err := s.acctRows(ctx, c)
	if err != nil {
		return 0, 0, nil, nil, err
	}

	memberSpend = map[string]float64{}
	cat, _ := s.Catalog(ctx)
	for _, j := range jobs {
		jt := jobTimestamp(j)
		if jt.IsZero() || jt.Before(start) || !jt.Before(resets) {
			continue
		}
		hrs := float64(j.Time.Elapsed) / 3600
		nodes := float64(max(j.AllocationNodes, 1))
		cores := float64(j.TRES.Allocated.Get("cpu"))
		price, priced := s.price(cat, j.Partition)
		cost := 0.0
		if priced {
			cost = price * nodes * hrs * allocShare(cat, j.Partition, int64(nodes), int64(cores), j.TRES.Allocated.Get("mem"))
		}
		if j.User == me {
			personUsed += cost
		}
		if isMember[j.User] {
			labUsed += cost
			memberSpend[j.User] += cost
		}
	}
	return personUsed, labUsed, labMembers, memberSpend, nil
}

// MyAllocation calculates the caller's allocation and current period spend.
func (s *Service) MyAllocation(ctx context.Context) (*MyAllocation, error) {
	now := s.Now()
	period, start, resets := periodBounds(now)
	out := &MyAllocation{
		Period:      period,
		PeriodStart: start.Format(time.RFC3339),
		ResetsAt:    resets.Format(time.RFC3339),
		Basis:       "list-price estimate",
		CoveredBy:   "Research Computing / UCR ITS",
		Enforcement: "warn",
		Notes:       []string{},
	}

	if s.Cfg.Allocation == nil {
		return out, nil
	}

	me, _ := s.User(ctx)
	personUsed, labUsed, labMembers, memberSpend, err := s.allocationUsage(ctx)
	if err != nil {
		return nil, err
	}

	if s.Cfg.Allocation.PersonUSD > 0 {
		used, left, pct, st := calcAllocation(s.Cfg.Allocation.PersonUSD, personUsed)
		out.Person = &PersonAllocation{
			AllocationUSD: round(s.Cfg.Allocation.PersonUSD, 2),
			UsedUSD:       used,
			LeftUSD:       left,
			PctUsed:       pct,
			Status:        st,
		}
	}

	if s.Cfg.Allocation.Lab != nil && s.Cfg.Allocation.Lab.AllocationUSD > 0 {
		used, left, pct, st := calcAllocation(s.Cfg.Allocation.Lab.AllocationUSD, labUsed)
		membersCount := len(labMembers)
		if membersCount == 0 {
			membersCount = 1
		}
		out.Lab = &LabAllocation{
			ID:            s.Cfg.Allocation.Lab.ID,
			Name:          s.Cfg.Allocation.Lab.Name,
			AllocationUSD: round(s.Cfg.Allocation.Lab.AllocationUSD, 2),
			UsedUSD:       used,
			LeftUSD:       left,
			PctUsed:       pct,
			Members:       membersCount,
			Status:        st,
		}
	}

	memberNotes := []string{}
	for _, m := range labMembers {
		if m != me {
			memberNotes = append(memberNotes, fmt.Sprintf("%s: $%.2f", m, round(memberSpend[m], 2)))
		}
	}
	out.Notes = memberNotes

	// Privacy guard: R1 callers never see per-member figures.
	// Only aggregate lab total and member count are exposed.
	if !hasTier(s.Cfg.Tiers, "R2") {
		out.Notes = []string{}
	}

	return out, nil
}

// planAllocation computes the allocation impact in a job_submit plan.
func (s *Service) planAllocation(ctx context.Context, worstUSD float64) *PlanAllocation {
	pa := &PlanAllocation{}
	if s.Cfg.Allocation == nil {
		return pa
	}
	hasPerson := s.Cfg.Allocation.PersonUSD > 0
	hasLab := s.Cfg.Allocation.Lab != nil && s.Cfg.Allocation.Lab.AllocationUSD > 0
	if !hasPerson && !hasLab {
		return pa
	}

	personUsed, labUsed, _, _, err := s.allocationUsage(ctx)
	if err != nil {
		// Never block job planning on allocation read failure
		return pa
	}

	if hasPerson {
		after := personUsed + worstUSD
		pct := round((after/s.Cfg.Allocation.PersonUSD)*100, 1)
		pa.PersonPctAfter = &pct
	}

	if hasLab {
		after := labUsed + worstUSD
		pct := round((after/s.Cfg.Allocation.Lab.AllocationUSD)*100, 1)
		pa.LabPctAfter = &pct
	}

	var warning string
	if pa.LabPctAfter != nil && *pa.LabPctAfter >= 80 {
		warning = fmt.Sprintf("This job could take your lab to %g%% of its Ursa allocation. Allocations are warn-only for now; it will still run.", *pa.LabPctAfter)
	} else if pa.PersonPctAfter != nil && *pa.PersonPctAfter >= 80 {
		warning = fmt.Sprintf("This job could take your personal allocation to %g%% of its Ursa allocation. Allocations are warn-only for now; it will still run.", *pa.PersonPctAfter)
	}
	pa.Warning = warning

	return pa
}
