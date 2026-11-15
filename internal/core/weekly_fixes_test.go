package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/rules"
)

func TestJobArrayRollupShowExplainAndWait(t *testing.T) {
	s, fx := newTestService(t)
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../testdata")); err != nil {
		t.Fatal(err)
	}
	// Append a 4-task array (parent 3000): 3000_0 COMPLETED, 3000_1 FAILED (separate stderr),
	// 3000_2 COMPLETED, 3000_3 PENDING in squeue initially.
	var acct map[string]any
	ab, _ := os.ReadFile(filepath.Join(dir, "sacct_jobs.json"))
	if err := json.Unmarshal(ab, &acct); err != nil {
		t.Fatal(err)
	}
	ajobs := acct["jobs"].([]any)
	mkAcctTask := func(jid, taskID int64, state string, rc int, outPath, errPath string) map[string]any {
		return map[string]any{
			"job_id":    jid,
			"array":     map[string]any{"job_id": 3000, "task_id": map[string]any{"set": true, "infinite": false, "number": taskID}},
			"name":      "render_arr",
			"user":      "alice_ucr_edu",
			"account":   "default",
			"partition": "gpul4",
			"state":     map[string]any{"current": []string{state}, "reason": "None"},
			"exit_code": map[string]any{
				"status":      []string{"ERROR"},
				"return_code": map[string]any{"set": true, "infinite": false, "number": rc},
			},
			"allocation_nodes":  1,
			"nodes":             "ucrslurmcl-gpul4nodeset-0",
			"time":              map[string]any{"elapsed": 42, "start": 1790870100, "end": 1790870142, "submission": 1790870000, "limit": map[string]any{"set": true, "number": 30}},
			"tres":              map[string]any{"allocated": []any{map[string]any{"type": "cpu", "count": 4}, map[string]any{"type": "mem", "count": 16384}}},
			"working_directory": "/home/alice_ucr_edu/bifrost-jobs/arr3000",
			"stdout_expanded":   outPath,
			"stderr_expanded":   errPath,
		}
	}
	ajobs = append(ajobs,
		mkAcctTask(3000, 0, "COMPLETED", 0, "/home/alice_ucr_edu/bifrost-jobs/arr3000/out_0.log", "/home/alice_ucr_edu/bifrost-jobs/arr3000/err_0.log"),
		mkAcctTask(3001, 1, "FAILED", 1, "/home/alice_ucr_edu/bifrost-jobs/arr3000/out_1.log", "/home/alice_ucr_edu/bifrost-jobs/arr3000/err_1.log"),
		mkAcctTask(3002, 2, "COMPLETED", 0, "/home/alice_ucr_edu/bifrost-jobs/arr3000/out_2.log", "/home/alice_ucr_edu/bifrost-jobs/arr3000/err_2.log"),
		mkAcctTask(3003, 3, "PENDING", 0, "/home/alice_ucr_edu/bifrost-jobs/arr3000/out_3.log", "/home/alice_ucr_edu/bifrost-jobs/arr3000/err_3.log"),
	)
	acct["jobs"] = ajobs
	ab, _ = json.Marshal(acct)
	_ = os.WriteFile(filepath.Join(dir, "sacct_jobs.json"), ab, 0o600)

	var sq map[string]any
	qb, _ := os.ReadFile(filepath.Join(dir, "squeue.json"))
	if err := json.Unmarshal(qb, &sq); err != nil {
		t.Fatal(err)
	}
	qjobs := sq["jobs"].([]any)
	qjobs = append(qjobs, map[string]any{
		"job_id":          3003,
		"array_job_id":    map[string]any{"set": true, "infinite": false, "number": 3000},
		"array_task_id":   map[string]any{"set": true, "infinite": false, "number": 3},
		"name":            "render_arr",
		"user_name":       "alice_ucr_edu",
		"partition":       "gpul4",
		"job_state":       []string{"PENDING"},
		"state_reason":    "Resources",
		"node_count":      map[string]any{"set": true, "number": 1},
		"cpus":            map[string]any{"set": true, "number": 4},
		"stdout_expanded": "/home/alice_ucr_edu/bifrost-jobs/arr3000/out_3.log",
		"stderr_expanded": "/home/alice_ucr_edu/bifrost-jobs/arr3000/err_3.log",
	})
	sq["jobs"] = qjobs
	qb, _ = json.Marshal(sq)
	_ = os.WriteFile(filepath.Join(dir, "squeue.json"), qb, 0o600)

	_ = os.WriteFile(filepath.Join(dir, "logs", "out_1.log"), []byte("starting frame 1\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "logs", "err_1.log"), []byte("CUDA out of memory: Tried to allocate 8.00 GiB\n"), 0o600)
	fx.Dir = dir
	fx.Files = map[string]map[string]string{
		"/home/alice_ucr_edu/bifrost-jobs/arr3000": {
			"out_0.log": "frame 0 ok\n",
			"out_1.log": "starting frame 1\n",
			"err_1.log": "CUDA out of memory: Tried to allocate 8.00 GiB\n",
			"out_2.log": "frame 2 ok\n",
		},
	}
	fx.Logs = map[string]string{
		"/home/alice_ucr_edu/bifrost-jobs/arr3000/out_1.log": "out_1.log",
		"/home/alice_ucr_edu/bifrost-jobs/arr3000/err_1.log": "err_1.log",
	}

	ctx := context.Background()

	// 1. JobsList(["3000"]) returns all 4 tasks; JobsList(["3000_1"]) returns only task 3001.
	allTasks, err := s.JobsList(ctx, JobsListInput{JobIDs: []string{"3000"}})
	if err != nil || len(allTasks) != 4 {
		t.Fatalf("JobsList([3000]): got %d tasks, err=%v: %+v", len(allTasks), err, allTasks)
	}
	oneTask, err := s.JobsList(ctx, JobsListInput{JobIDs: []string{"3000_1"}})
	if err != nil || len(oneTask) != 1 || oneTask[0].JobID != "3001" || oneTask[0].ArrayTaskID != "1" {
		t.Fatalf("JobsList([3000_1]): got %+v, err=%v", oneTask, err)
	}

	// 2. JobShow("3000_1") returns task 3001 specifically; JobShow("3000") rolls up all 4 tasks and selects the failed task!
	d1, err := s.JobShow(ctx, JobShowInput{JobID: "3000_1"})
	if err != nil || d1.JobID != "3001" || d1.State != "FAILED" {
		t.Fatalf("JobShow(3000_1): %+v err=%v", d1, err)
	}
	dp, err := s.JobShow(ctx, JobShowInput{JobID: "3000"})
	if err != nil || dp.Array == nil {
		t.Fatalf("JobShow(3000) missing Array summary: %+v err=%v", dp, err)
	}
	if dp.Array.Total != 4 || dp.Array.Completed != 2 || dp.Array.Failed != 1 || dp.Array.Pending != 1 {
		t.Fatalf("Array summary mismatch: %+v", dp.Array)
	}
	if len(dp.Array.FailedTasks) != 1 || dp.Array.FailedTasks[0] != "3000_1" {
		t.Fatalf("FailedTasks mismatch: %+v", dp.Array.FailedTasks)
	}

	// 3. JobExplain("3000") diagnoses the failed task's stderr (CUDA out of memory) instead of failing on pending task 3003!
	ex, err := s.JobExplain(ctx, "3000", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Findings) == 0 || ex.Findings[0].Rule != "cuda" {
		t.Fatalf("expected cuda finding from task 3000_1 stderr, got: %+v", ex.Findings)
	}

	// 4. JobWait(["3000"]) does NOT exit early while task 3003 is PENDING; times out cleanly with Array summary!
	polls := 0
	wPending, err := s.JobWait(ctx, JobWaitInput{
		JobIDs:   []string{"3000"},
		TimeoutS: 1,
		PollS:    5,
		OnPoll:   func(_ int64, active, done int) { polls++; _ = active; _ = done },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !wPending.TimedOut || polls == 0 || wPending.Jobs[0].State != "PENDING" {
		t.Fatalf("expected JobWait to keep waiting while 3000_3 is PENDING: %+v (polls=%d)", wPending, polls)
	}

	// Now finish 3000_3 (remove from squeue, mark COMPLETED in sacct) and verify JobWait returns FAILED with stderr tail!
	sq["jobs"] = qjobs[:len(qjobs)-1]
	qb, _ = json.Marshal(sq)
	_ = os.WriteFile(filepath.Join(dir, "squeue.json"), qb, 0o600)
	ajobs[len(ajobs)-1] = mkAcctTask(3003, 3, "COMPLETED", 0, "/home/alice_ucr_edu/bifrost-jobs/arr3000/out_3.log", "/home/alice_ucr_edu/bifrost-jobs/arr3000/err_3.log")
	acct["jobs"] = ajobs
	ab, _ = json.Marshal(acct)
	_ = os.WriteFile(filepath.Join(dir, "sacct_jobs.json"), ab, 0o600)

	wDone, err := s.JobWait(ctx, JobWaitInput{JobIDs: []string{"3000"}, TimeoutS: 5, PollS: 5})
	if err != nil {
		t.Fatal(err)
	}
	if wDone.TimedOut || wDone.Jobs[0].State != "FAILED" || wDone.Jobs[0].Array == nil || wDone.Jobs[0].Array.Completed != 3 || wDone.Jobs[0].Array.Failed != 1 {
		t.Fatalf("JobWait finished array mismatch: %+v (array=%+v)", wDone.Jobs[0], wDone.Jobs[0].Array)
	}
	if wDone.Jobs[0].LogTail == nil || !strings.Contains(wDone.Jobs[0].LogTail.Text, "CUDA out of memory") || !strings.Contains(wDone.Jobs[0].LogTail.Text, "--- stderr ---") {
		t.Fatalf("JobWait failed array task missing combined stdout+stderr log tail: %+v", wDone.Jobs[0].LogTail)
	}
	if DefaultWaitTimeoutS != 120 {
		t.Fatalf("DefaultWaitTimeoutS = %d, want 120", DefaultWaitTimeoutS)
	}
}

func TestUploadBatchInlineAndUnpackArchives(t *testing.T) {
	s, fx, _ := stagedService(t, "alice@ucr.edu")
	ctx := context.Background()

	// 1. Inline text upload stages directly without a separate curl step.
	tkText, err := s.UploadPrepareSpec(ctx, UploadFileSpec{Filename: "config.json", ContentText: `{"epochs":10}`})
	if err != nil {
		t.Fatal(err)
	}
	if !tkText.Uploaded || tkText.URL != "" || tkText.UploadID == "" {
		t.Fatalf("inline text upload ticket: %+v", tkText)
	}

	// 2. Inline base64 upload works; > MaxInlineUploadBytes is refused with ErrCapExceeded.
	rawZip := []byte("PK\x03\x04fakezip")
	tkB64, err := s.UploadPrepareSpec(ctx, UploadFileSpec{
		Filename:      "assets.zip",
		ContentBase64: base64.StdEncoding.EncodeToString(rawZip),
	})
	if err != nil || !tkB64.Uploaded {
		t.Fatalf("inline base64 upload: %+v err=%v", tkB64, err)
	}
	tooBig := strings.Repeat("x", MaxInlineUploadBytes+1)
	if _, err := s.UploadPrepareSpec(ctx, UploadFileSpec{Filename: "big.txt", ContentText: tooBig}); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("expected ErrCapExceeded for >256KB inline upload, got: %v", err)
	}

	// 3. Batch upload prepares/stages multiple files, enforces user quota, and rejects duplicate filenames.
	if _, err := s.UploadPrepareBatch(ctx, []UploadFileSpec{{Filename: "a.txt", Bytes: 10}, {Filename: "a.txt", Bytes: 20}}); err == nil {
		t.Fatal("duplicate filenames in batch should fail")
	}
	origUserCap := s.Cfg.Staging.MaxUserBytes
	s.Cfg.Staging.MaxUserBytes = 1000
	if _, err := s.UploadPrepareBatch(ctx, []UploadFileSpec{
		{Filename: "q1.bin", Bytes: 600},
		{Filename: "q2.bin", Bytes: 600},
	}); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("expected ErrCapExceeded when batch exceeds MaxUserBytes, got: %v", err)
	}
	s.Cfg.Staging.MaxUserBytes = origUserCap
	batch, err := s.UploadPrepareBatch(ctx, []UploadFileSpec{
		{Filename: "bundle.tar.gz", ContentBase64: base64.StdEncoding.EncodeToString([]byte("tarbytes"))},
		{Filename: "weights.bin", Bytes: 4096},
	})
	if err != nil || len(batch.Uploads) != 2 || !batch.Uploads[0].Uploaded || batch.Uploads[1].Uploaded {
		t.Fatalf("batch upload result: %+v err=%v", batch, err)
	}

	// 4. PrepareSubmit + ConfirmSubmit with UnpackArchives=true injects tar/zip extraction and covers Unpack in plan hash.
	script := "#!/bin/bash\n#SBATCH -p standard\n#SBATCH -c 2\n#SBATCH -t 10\nls inputs/\n"
	planPlain, err := s.PrepareSubmit(ctx, SubmitInput{Script: script, Inputs: []string{batch.Uploads[0].UploadID, tkB64.UploadID}, UnpackArchives: false})
	if err != nil {
		t.Fatal(err)
	}
	planUnpack, err := s.PrepareSubmit(ctx, SubmitInput{Script: script, Inputs: []string{batch.Uploads[0].UploadID, tkB64.UploadID}, UnpackArchives: true})
	if err != nil {
		t.Fatal(err)
	}
	if planPlain.PlanHash == planUnpack.PlanHash {
		t.Fatalf("UnpackArchives must change plan hash: %s vs %s", planPlain.PlanHash, planUnpack.PlanHash)
	}
	if _, err := s.ConfirmSubmit(ctx, planUnpack.Token); err != nil {
		t.Fatal(err)
	}
	submitted := string(fx.Stdin[len(fx.Stdin)-1])
	if !strings.Contains(submitted, "tar -xzf inputs/bundle.tar.gz -C inputs") || !strings.Contains(submitted, "python3 -m zipfile -e inputs/assets.zip inputs") {
		t.Fatalf("submitted script missing archive unpack commands:\n%s", submitted)
	}
}

func TestResultsLinkArchiveAndFilesLink(t *testing.T) {
	s, fx, _ := stagedService(t, "alice@ucr.edu")
	dir := "/home/alice_ucr_edu/deep-research-lab/run_76"
	fx.Files = map[string]map[string]string{
		dir: {
			"frames/f01.png": "png1",
			"frames/f02.png": "png2",
			"metrics.json":   "{}",
		},
	}
	homeFixture(fx)
	fx.Paths["/apps/docs/TPU_PORTING.md"] = "# TPU Guide\n"
	ctx := context.Background()

	// 1. ResultsLinkOpts with Archive=true + Pattern="*.png" streams a single tar.gz with zero temp files.
	rl, err := s.ResultsLinkOpts(ctx, ResultsLinkInput{JobID: "236", Pattern: "*.png", Archive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rl.Links) != 1 || rl.Links[0].File != "job-236-results.tar.gz" || !strings.Contains(rl.Links[0].URL, "storage.googleapis.com") {
		t.Fatalf("archive results_link: %+v", rl)
	}

	// 2. FilesLink works for home/scratch regular files (single + archive) and blocks sensitive/hidden/outside paths.
	fl, err := s.FilesLink(ctx, FilesLinkInput{Paths: []string{"~/project/notes.txt", "/scratch/alice_ucr_edu/big/out.dat"}})
	if err != nil || len(fl.Links) != 2 {
		t.Fatalf("FilesLink single: %+v err=%v", fl, err)
	}
	flArch, err := s.FilesLink(ctx, FilesLinkInput{Paths: []string{"~/project/notes.txt", "./project/run.sbatch"}, Archive: true})
	if err != nil || len(flArch.Links) != 1 || !strings.HasSuffix(flArch.Links[0].File, ".tar.gz") {
		t.Fatalf("FilesLink archive: %+v err=%v", flArch, err)
	}
	for _, bad := range []string{"/etc/passwd", "/home/bob_ucr_edu/notes.txt", "~/.ssh/id_ed25519", "~/project/aws.pem", "~/project/escape", "/apps/docs/TPU_PORTING.md"} {
		if _, err := s.FilesLink(ctx, FilesLinkInput{Paths: []string{bad}}); err == nil {
			t.Errorf("FilesLink accepted forbidden path %q", bad)
		}
	}
	origLinkCap := s.Cfg.Staging.MaxLinkBytes
	s.Cfg.Staging.MaxLinkBytes = 5
	if _, err := s.FilesLink(ctx, FilesLinkInput{Paths: []string{"~/project/notes.txt"}}); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("expected ErrCapExceeded for FilesLink over MaxLinkBytes, got: %v", err)
	}
	s.Cfg.Staging.MaxLinkBytes = origLinkCap
}

func TestPublicDocsReadAndDotNormalization(t *testing.T) {
	s, fx := newTestService(t)
	homeFixture(fx)
	fx.Paths["/apps/docs/TPU_PORTING.md"] = "# TPU v6e Porting Guide\n"
	fx.Paths["/apps/containers/cuda-12.4-devel.sif"] = "\x7fELFsif"
	fx.Links["/apps/docs/evil_link"] = "/etc/passwd"
	ctx := context.Background()

	// 1. "." and "./project/." normalize cleanly; ".." still fails with ErrDenied.
	if l, err := s.FilesList(ctx, FilesListInput{Path: "."}); err != nil || l.Path != "/home/alice_ucr_edu" {
		t.Fatalf("FilesList(.): %+v err=%v", l, err)
	}
	if r, err := s.FilesRead(ctx, FilesReadInput{Path: "./project/./notes.txt"}); err != nil || !strings.HasPrefix(r.Text.Text, "hello") {
		t.Fatalf("FilesRead(./project/./notes.txt): %+v err=%v", r, err)
	}
	if _, err := s.FilesRead(ctx, FilesReadInput{Path: "./../bob_ucr_edu/notes.txt"}); !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("traversal ./../ must still return ErrDenied, got: %v", err)
	}

	// 2. /apps/docs is readable & listable; /apps/containers is listable, binary read gives apptainer hint; symlink out of /apps/docs is denied!
	if dl, err := s.FilesList(ctx, FilesListInput{Path: "/apps/containers"}); err != nil || len(dl.Entries) != 1 {
		t.Fatalf("FilesList(/apps/containers): %+v err=%v", dl, err)
	}
	if dr, err := s.FilesRead(ctx, FilesReadInput{Path: "/apps/docs/TPU_PORTING.md"}); err != nil || !strings.Contains(dr.Text.Text, "TPU v6e") {
		t.Fatalf("FilesRead(/apps/docs/TPU_PORTING.md): %+v err=%v", dr, err)
	}
	if _, err := s.FilesRead(ctx, FilesReadInput{Path: "/apps/containers/cuda-12.4-devel.sif"}); !errors.Is(err, policy.ErrDenied) || !strings.Contains(err.Error(), "apptainer exec") {
		t.Fatalf("FilesRead(/apps/containers/*.sif) should give apptainer hint, got: %v", err)
	}
	if _, err := s.FilesRead(ctx, FilesReadInput{Path: "/apps/docs/evil_link"}); !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("symlink escaping /apps/docs must be denied: %v", err)
	}
}

func TestScriptCheckMemPerCPUFallbackAndStatusFilter(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	// 1. --mem-per-cpu * cpus exceeding node RAM fails static check.
	mpcScript := "#!/bin/bash\n#SBATCH -p standard\n#SBATCH --cpus-per-task=16\n#SBATCH --mem-per-cpu=8G\n#SBATCH -t 10\necho hi\n"
	sc, err := s.ScriptCheck(ctx, mpcScript)
	if err != nil {
		t.Fatal(err)
	}
	if sc.OK || !strings.Contains(issues(sc), "--mem-per-cpu 8G x 16 CPUs (128.0 GB) is more than a standard node has") {
		t.Fatalf("expected --mem-per-cpu overflow error, got: ok=%v issues=%s", sc.OK, issues(sc))
	}

	// 2. Fallback partition list checks every member's core/memory/GPU limits, not just the dearest.
	fallbackScript := "#!/bin/bash\n#SBATCH -p computehigh,standard\n#SBATCH --cpus-per-task=32\n#SBATCH -t 10\necho hi\n"
	sc, err = s.ScriptCheck(ctx, fallbackScript)
	if err != nil {
		t.Fatal(err)
	}
	if sc.OK || !strings.Contains(issues(sc), "fallback partition standard in computehigh,standard only has 16 physical cores") {
		t.Fatalf("expected fallback partition core overflow error, got: ok=%v issues=%s", sc.OK, issues(sc))
	}

	// 3. ScriptCheckWithPartition applies partition override.
	overrideSc, err := s.ScriptCheckWithPartition(ctx, "#!/bin/bash\n#SBATCH -t 10\necho hi\n", "gpul4")
	if err != nil || !strings.Contains(issues(overrideSc), "without --gres=gpu:1") {
		t.Fatalf("ScriptCheckWithPartition(gpul4): %+v issues=%s err=%v", overrideSc, issues(overrideSc), err)
	}

	// 4. ClusterStatusFiltered narrows partitions and reports SuspendMin.
	cs, err := s.ClusterStatusFiltered(ctx, "gpul4")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Partitions) != 1 || cs.Partitions[0].Name != "gpul4" || cs.Partitions[0].SuspendMin != 30 {
		t.Fatalf("ClusterStatusFiltered(gpul4): %+v", cs.Partitions)
	}
	if _, err := s.ClusterStatusFiltered(ctx, "nonexistent"); err == nil {
		t.Fatal("ClusterStatusFiltered(nonexistent) should error")
	}

	// 5. Rules: requeued pending with Resources reason + script-error fallback includes untrusted log tail lines.
	fs := rules.Explain(rules.Facts{State: "PENDING", Reason: "Resources", RestartCnt: 3})
	if len(fs) == 0 || fs[0].Rule != "requeued" {
		t.Fatalf("expected requeued rule on RestartCnt=3 + Resources, got: %+v", fs)
	}
	fsFail := rules.Explain(rules.Facts{State: "FAILED", ExitCode: "2", LogReadable: true, Log: "setup ok\ncustom solver exploded: NaN at step 9\n"})
	if len(fsFail) == 0 || fsFail[0].Rule != "script-error" || !strings.Contains(strings.Join(fsFail[0].Evidence, "\n"), "untrusted log line: custom solver exploded") {
		t.Fatalf("expected script-error to quote last log lines: %+v", fsFail)
	}
}
