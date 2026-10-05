#!/usr/bin/env bash
# Mutation check: disable each safety guard in turn and confirm a test fails.
# Restores every file from a backup copy in a trap. Run: scripts/mutation_check.sh
set -u
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.26.8} PYTHONDONTWRITEBYTECODE=1
BK=$(mktemp -d)
FILES="internal/core/inflight.go internal/core/shared_cache.go internal/policy/audit.go internal/core/jobs.go internal/backend/command.go internal/core/service.go internal/policy/redact.go internal/core/a1.go internal/core/results.go internal/slurm/types.go internal/core/cluster.go internal/backend/iap.go internal/rules/rules.go internal/server/oauth.go internal/server/server.go internal/server/store.go internal/server/google.go internal/core/files.go internal/core/staged.go internal/core/helpers.go internal/backend/files.go internal/staging/staging.go internal/core/p2.go internal/core/shared.go internal/config/config.go internal/core/catalog.go internal/core/dbcheck.go internal/slurm/sacctrows.go"
for f in $FILES; do mkdir -p "$BK/$(dirname "$f")"; cp "$f" "$BK/$f"; done
restore() { for f in $FILES; do cp "$BK/$f" "$f"; done; }
trap restore EXIT

mutate() { # name file python-replace(old,new)
  local name=$1 file=$2 old=$3 new=$4
  # ONLY=<prefix> runs just the guards whose name starts with it (e.g. ONLY=v093)
  case "$name" in "${ONLY:-}"*) ;; *) return;; esac
  case " $FILES " in *" $file "*) ;; *) echo "ERROR $name: $file is not in FILES (would not be restored)"; exit 2;; esac
  if python3 - "$file" "$old" "$new" <<'EOF'
import sys
p, old, new = sys.argv[1:]
t = open(p).read()
assert t.count(old) == 1, f"pattern not unique in {p}: {old!r}"
open(p, "w").write(t.replace(old, new))
EOF
  then :; else echo "BROKEN    $name (pattern did not apply; nothing was tested)"; FAIL=1; restore; return; fi
  if go test -count=1 ./... >/dev/null 2>&1; then echo "SURVIVED  $name"; FAIL=1; else echo "killed    $name"; fi
  restore
}
FAIL=0
mutate "tier check"        internal/policy/audit.go   'if t == tier {' 'if true {'
mutate "owner check"       internal/core/jobs.go      'if owner != me && !in.AnyUser {' 'if false {'
mutate "log root check"    internal/core/jobs.go      'if strings.HasPrefix(clean, r) {' 'if true {'
mutate "job id validation" internal/backend/command.go 'if !regexp.MustCompile(`^\d{1,10}(_\d{1,7})?$`).MatchString(id) {' 'if false {'
mutate "quoting"           internal/backend/command.go 'return "'"'"'" + strings.ReplaceAll(s, "'"'"'", `'"'"'\'"'"''"'"'`) + "'"'"'"' 'return s'
mutate "redaction"         internal/policy/redact.go  's = r.re.ReplaceAllString(s, r.repl)' '_ = r'
mutate "log line cap"      internal/core/jobs.go      'lines = s.Cfg.Limits.LogLines' 'lines = lines'
mutate "rate limit"        internal/policy/audit.go   'if l.tokens < 1 {' 'if false {'
mutate "script redaction"  internal/core/jobs.go      'd.Script = policy.Wrap(j.Script, s.Cfg.Limits.ScriptBytes)' 'd.Script = \&policy.Untrusted{Note: policy.UntrustedNote, Text: j.Script}'
mutate "A1 single-use token"  internal/core/a1.go 'delete(st.Pending, key) // single use, even when it then fails' '_ = key'
mutate "A1 token expiry"      internal/core/a1.go 'if s.Now().After(p.Expires) {
		return pending{}, errors.New("confirm_token expired' 'if false {
		return pending{}, errors.New("confirm_token expired'
mutate "A1 plan hash check"   internal/core/a1.go 'if submitHash(p.Dir, p.Opts, hashOf(p.Script), inputsHash(p.Inputs)) != p.Hash {' 'if submitHash(p.Dir, p.Opts, hashOf(p.Script), inputsHash(p.Inputs)) == "never" {'
mutate "MPI prerequisite"     internal/core/cluster.go 'if loadedBefore(lines, mpi) {' 'if true {'
mutate "A1 node cap"          internal/core/a1.go 'if nodes > caps.MaxNodes {' 'if false {'
mutate "A1 hour cap"          internal/core/a1.go 'if float64(mins) > caps.MaxHours*60 {' 'if false {'
mutate "A1 job cost cap"      internal/core/a1.go 'if worst > caps.MaxCostPerJobUSD {' 'if false {'
mutate "A1 day cap (prepare)" internal/core/a1.go 'if spent+worst > caps.MaxCostPerDayUSD {' 'if false {'
mutate "A1 day cap (confirm)" internal/core/a1.go 'if spent+p.WorstUSD > s.Cfg.Caps.MaxCostPerDayUSD {' 'if false {'
mutate "A1 time required"     internal/core/a1.go 'if !ok || mins <= 0 {' 'if false {'
mutate "A1 kind check"        internal/core/a1.go 'if p.Kind != kind {' 'if false {'
mutate "results traversal"    internal/core/results.go 'if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {' 'if false {'
mutate "results no overwrite" internal/core/results.go 'target = freeName(target)' 'target = target'
mutate "results home guard"   internal/core/results.go 'if dir == home || dir == home+"/" {' 'if false {'
mutate "A1 cross-process lock" internal/core/a1.go 'if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {' 'if false {'
mutate "redact whole assignment" internal/policy/redact.go 'for _, r := range redactRules {' 'for _, r := range append(append([]redactRule{}, redactRules[1:]...), redactRules[0]) {'
mutate "booting is not broken" internal/slurm/types.go 'return n.HasState("NOT_RESPONDING") && !n.Booting()' 'return n.HasState("NOT_RESPONDING")'
mutate "release only never-ran" internal/core/a1.go 'neverRan := d.State == "PENDING" && d.Started == ""' 'neverRan := true'
mutate "iap host key pin"     internal/backend/iap.go 'if err == nil && bytes.Equal(pk.Marshal(), key.Marshal()) {' 'if true {'
mutate "iap key id check"     internal/backend/iap.go 'if _, ok := r.LoginProfile.SSHPublicKeys[keyID(line)]; !ok {' 'if false {'
mutate "iap single flight"    internal/backend/iap.go '	b.connMu.Lock()
	defer b.connMu.Unlock()' '	// no lock'
mutate "iap reuse margin"     internal/backend/iap.go 'if k != nil && time.Until(k.Expires) > keyMinLife {' 'if k != nil {'
mutate "iap no write retry"   internal/backend/iap.go '		if c.write {
			// never re-run' '		if false {
			// never re-run'
mutate "srv user list"        internal/server/oauth.go 'if s.users.Lookup(id.Email) == nil {' 'if false {'
mutate "srv domain check"     internal/server/oauth.go 'if !id.EmailVerified || id.HD != s.users.Domain() {' 'if false {'
mutate "srv pkce"             internal/server/oauth.go 'case f.Get("code_verifier") == "" || s256(f.Get("code_verifier")) != c.CodeChallenge:' 'case false:'
mutate "srv code single use"  internal/server/oauth.go 'delete(s.auth.codes, hashTok(f.Get("code"))) // single use' '_ = 0'
mutate "srv refresh client"   internal/server/oauth.go 'if rec.ClientID != f.Get("client_id") {' 'if false {'
mutate "srv refresh rotate"   internal/server/oauth.go '_ = s.store.Delete("refresh", hashTok(rt)) // rotate' '_ = 0'
mutate "srv redirect check"   internal/server/oauth.go 'if !okRU {' 'if false {'
mutate "srv per-request user" internal/server/oauth.go '	u := s.users.Lookup(a.Email)
	if u == nil {' '	u := &User{Email: a.Email, Tiers: []string{"R1", "R2", "A1"}}
	if u == nil {'
mutate "srv per-user tiers"   internal/server/server.go 'c.Tiers = append([]string(nil), u.Tiers...)' 'c.Tiers = []string{"R1", "R2", "A1"}'
mutate "srv remote download"  internal/core/results.go 'if in.Download && s.Remote {' 'if false {'
mutate "srv seal label"       internal/server/store.go 'pt, err := s.aead.Open(nil, b[:n], b[n:], []byte(label))' 'pt, err := s.aead.Open(nil, b[:n], b[n:], nil)'
mutate "srv cloud scope"      internal/server/oauth.go 'if !strings.Contains(tok.Scope, "https://www.googleapis.com/auth/cloud-platform") {' 'if false {'
mutate "srv id audience"      internal/server/google.go 'case c.Aud != g.ClientID:' 'case false:'
mutate "srv stored token session" internal/server/oauth.go '	if found, _ := s.store.Get("session", a.Email, &session{}); !found {
		return accessRec{}, false
	}
	s.auth.mu.Lock()' '	s.auth.mu.Lock()'
mutate "srv consent retry"     internal/server/oauth.go '		if !p.Consent {
			http.Redirect(w, r, s.googleURL(p, true), http.StatusFound)
			return
		}' ''
mutate "srv no forced consent" internal/server/oauth.go '	prompt := "select_account"' '	prompt := "consent"'
mutate "rule cmd-not-found case" internal/rules/rules.go '(?mi)(\S+): command not found\s*$' '(?m)(\S+): command not found$'
mutate "rule exit 127"           internal/rules/rules.go 'if st == "FAILED" && f.ExitCode == "127" && !hasRule(out, "command-not-found") {' 'if false {'
mutate "srv whoami auth"        internal/server/server.go '	a, u, err := s.verifyAccess(tok)
	if err != nil {
		s.challenge(w, err.Error())
		return
	}
	p, err := s.programFor(a.ClientID)' '	a, u := \&accessRec{}, s.users.Lookup("bob@ucr.edu")
	p, err := s.programFor(a.ClientID)'
mutate "v07 user root check"     internal/core/files.go 'if p == r {
			return nil
		}
		if strings.HasPrefix(p, r+"/") {' 'if p == r {
			return nil
		}
		if true {'
mutate "v07 hidden component"     internal/core/files.go '				if hiddenOrSensitive(part) {' '				if false {'
mutate "v07 realpath recheck"     internal/core/files.go '	if err := checkUserPath(real, roots); err != nil {' '	if err := checkUserPath(p, roots); err != nil {'
mutate "v07 listing hides"        internal/core/files.go '		if hiddenOrSensitive(name) {
			l.Hidden++' '		if false {
			l.Hidden++'
mutate "v07 sensitive names"      internal/core/files.go 'func sensitive(name string) bool { return sensitiveName.MatchString(name) }' 'func sensitive(name string) bool { return false }'
mutate "v07 chunk redaction"      internal/core/files.go '	r.Chunk, r.Text = info, policy.Wrap(text, 0)' '	r.Chunk, r.Text = info, \&policy.Untrusted{Text: text}'
mutate "v07 grep redaction"       internal/core/files.go '		Lines: policy.Wrap(rest, 0)}, nil' '		Lines: \&policy.Untrusted{Text: rest}}, nil'
mutate "v07 binary refusal"       internal/core/files.go '	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {' '	if false {'
mutate "v07 utf8 boundary"        internal/core/files.go '	for len(b) > 0 && offset > 0 && b[0]&0xC0 == 0x80 {' '	for false {'
mutate "v07 results read in listing" internal/core/results.go '		if !hasFile(all, in.Read) {' '		if false {'
mutate "v07 results prefix check" internal/core/results.go '		if err := backend.ValidRelPath(in.Prefix); err != nil {' '		if err := error(nil); err != nil {'
mutate "v07 owner key from identity" internal/core/staged.go '	who := strings.ToLower(strings.TrimSpace(s.Principal))' '	who := "everyone"'
mutate "v07 upload name check"    internal/core/staged.go '	if err := ValidUploadName(filename); err != nil {
		return nil, err
	}
	sc := s.Cfg.Staging' '	sc := s.Cfg.Staging'
mutate "v07 upload size cap"      internal/core/staged.go '	if size > sc.MaxUploadBytes {' '	if false {'
mutate "v07 user quota"           internal/core/staged.go '	if used+size > sc.MaxUserBytes {' '	if false {'
mutate "v07 length-range header"  internal/core/staged.go '	hdr := map[string]string{"x-goog-content-length-range": "0," + strconv.FormatInt(size, 10)}' '	hdr := map[string]string{}'
mutate "v07 input id format"      internal/core/staged.go '		if !reUploadID.MatchString(id) {
			return nil, fmt.Errorf("input' '		if false {
			return nil, fmt.Errorf("input'
mutate "v07 input generation"     internal/core/staged.go '		if !ok || o.Generation != f.Gen || o.Size != f.Bytes {' '		if !ok {'
mutate "v07 inputs in plan hash"  internal/core/staged.go '		parts = append(parts, f.Object, f.Gen, strconv.FormatInt(f.Bytes, 10))' '		_ = f'
mutate "v07 link file in listing" internal/core/staged.go '		if !hasFile(all, f) {
			return nil, fmt.Errorf("%s is not a file in the job folder (see job_results)", f)' '		if false {
			return nil, fmt.Errorf("%s is not a file in the job folder (see job_results)", f)'
mutate "v07 link size cap"        internal/core/staged.go '	if total > s.Cfg.Staging.MaxLinkBytes {' '	if false {'
mutate "v07 fetch after header"   internal/core/staged.go '	return insertAfterHeader(script, fetchBlock(urls, names)), nil' '	return fetchBlock(urls, names) + script, nil'
mutate "v07 env version allowlist" internal/core/helpers.go '	c, err := backend.EnvCheck(modules, commands, versionCommands)' '	c, err := backend.EnvCheck(modules, commands, map[string]bool{"myprog": true, "gcc": true, "python3": true})'
mutate "v07 interactive gpus"     internal/core/helpers.go '	if in.GPUs < 0 || in.GPUs > cp.GPUsPerNode {' '	if in.GPUs < 0 {'
mutate "v07 interactive memory"   internal/core/helpers.go '	if in.Memory != "" && !reMemory.MatchString(in.Memory) {' '	if false {'
mutate "v07 storage hides creds"  internal/core/helpers.go '		if sensitive(path.Base(p)) {' '		if false {'
mutate "v07 grep pattern check"   internal/backend/files.go '	if p == "" || len(p) > 200 || strings.ContainsAny(p, "\x00\n\r") {' '	if p == "" {'
mutate "v07 env command names"    internal/backend/files.go '		if err := ValidCommandName(c); err != nil {
			return Command{}, err
		}' ''
mutate "v07 signed url check"     internal/backend/files.go '	if len(u) > 4000 || !reSignedURL.MatchString(u) || !strings.Contains(u, "X-Goog-Signature=") {' '	if false {'
mutate "v07 read size cap"        internal/backend/files.go '	if n < 1 || n > MaxReadBytes {' '	if n < 1 {'
mutate "v07 sign method"          internal/staging/staging.go '	if method != http.MethodGet && method != http.MethodPut {' '	if false {'
mutate "v07 sign ttl"             internal/staging/staging.go '	if ttl < time.Second || ttl > MaxTTL {' '	if false {'
mutate "v07 sign object"          internal/staging/staging.go '	if object == "" || strings.HasPrefix(object, "/") || strings.Contains(object, "..") {' '	if false {'
mutate "v07 signature redaction"  internal/policy/redact.go '	{regexp.MustCompile(`(?i)(X-Goog-Signature=)[0-9a-f]{16,}`), "${1}[REDACTED]"},' ''
mutate "v07 trace redaction"      internal/core/service.go '		t.cmds = append(t.cmds, policy.Redact(key))' '		t.cmds = append(t.cmds, key)'
mutate "v071 ssh session slots"   internal/backend/iap.go '	release, err := b.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()' ''
mutate "v071 refused channel keeps conn" internal/backend/iap.go '			if errors.As(err, &oce) {' '			if false && errors.As(err, &oce) {'
mutate "v071 unreachable not missing" internal/core/files.go '	if errors.Is(err, backend.ErrUnreachable) {
		return "", err // a transport problem, not a missing file
	}' ''
mutate "v072 module_show loads MPI"   internal/core/cluster.go 'c, err := backend.ModuleShowUnder(name, res.LoadedFirst)' 'c, err := backend.ModuleShowUnder(name, "")'
mutate "v072 module_show prereq validated" internal/backend/command.go '		if !reModule.MatchString(prereq) {' '		if false {'
mutate "v072 module_show mpi must match" internal/core/cluster.go '		if !ok {
			if len(choices) == 0 {' '		if false {
			if len(choices) == 0 {'
mutate "v072 completed job not a failure" internal/core/p2.go '	return j.State == "COMPLETED" && (j.ExitCode == "0" || j.ExitCode == "")' '	return false'
mutate "v072 failed job not a success"   internal/core/p2.go '	return j.State == "COMPLETED" && (j.ExitCode == "0" || j.ExitCode == "")' '	return j.ExitCode == "0" || j.ExitCode == ""'
mutate "v072 jobs_list config cap"       internal/core/jobs.go '	if limit > s.Cfg.Limits.ListRows {
		limit = s.Cfg.Limits.ListRows
	}
	var qc' '	var qc'
mutate "v080 no-core error on shared"   internal/core/cluster.go '		if shared && !cr.CoresAsked {' '		if false {'
mutate "v080 submit re-checks override"  internal/core/a1.go '	} else if s.partitionShared(ctx, part) && !coresAsked(req) {' '	} else if false {'
mutate "v080 shared price share"         internal/core/a1.go '		share = coreRequest(req, cp, true).NodeShare' '		share = 1'
mutate "v080 exclusive pays the node"    internal/core/shared.go '	if r.Exclusive || !shared {' '	if !shared {'
mutate "v080 memory share counts"        internal/core/shared.go '		if m := float64(mem) / float64(nodeMemMB); m > share {' '		if m := 0.0; m > share {'
mutate "v080 memory capped at node"     internal/core/shared.go '	if nodeMemMB > 0 && mem > nodeMemMB {
		mem = nodeMemMB
	}' ''
mutate "v080 cores capped at node"       internal/core/shared.go '	if nodeCores > 0 && cores > nodeCores {
		cores = nodeCores
	}' ''
mutate "v080 EXCLUSIVE is not shared"    internal/core/shared.go '		out[strings.TrimSuffix(name, "*")] = mode != "" && mode != "EXCLUSIVE"' '		out[strings.TrimSuffix(name, "*")] = mode != ""'
mutate "v080 failed read is exclusive"   internal/core/shared.go '	if err != nil {
		return out
	}
	for _, line' '	if err != nil {
		return map[string]bool{"computehigh": true}
	}
	for _, line'
mutate "v080 every core request counts"  internal/core/shared.go '	for _, k := range []string{"cpus-per-task", "ntasks", "ntasks-per-node", "exclusive"} {' '	for _, k := range []string{"cpus-per-task"} {'
mutate "v080 script_check share price"   internal/core/cluster.go '			c := round(price*float64(nodes)*cr.NodeShare*float64(mins)/60, 2)' '			c := round(price*float64(nodes)*float64(mins)/60, 2)'
mutate "v081 no-core error wording"     internal/core/cluster.go 'or #SBATCH --exclusive for the whole node", part, cr.DefaultNote)' 'or #SBATCH --exclusive for the whole node", part, strings.TrimPrefix(cr.DefaultNote, "asks for no cores, so Slurm gives it "))'
mutate "v081 commented module load"     internal/core/cluster.go '		if strings.HasPrefix(ln, "#") {
			continue // a comment that mentions `module load x` loads nothing
		}' ''
mutate "v081 run-time checks skip comments" internal/core/cluster.go '	code := codeOnly(script)' '	code := script'
mutate "v080 usage cost share"           internal/core/cluster.go '				x.row.CostUSD += price * nodes * hrs * allocShare(cat, j.Partition, int64(nodes), int64(cores), j.TRES.Allocated.Get("mem"))' '				x.row.CostUSD += price * nodes * hrs'
mutate "v080 job list cost share"        internal/core/jobs.go '		share := allocShare(cat, j.Partition, n, js.CPUs, j.TRES.Allocated.Get("mem"))' '		share := 1.0'
mutate "v080 waste node-hours share"    internal/core/p2.go '		share := allocShare(cat, j.Partition, int64(nodes), int64(cores), j.TRES.Allocated.Get("mem"))' '		share := 1.0'
mutate "v080 alloc share capped"         internal/core/shared.go '	if share <= 0 || share > 1 {' '	if share <= 0 {'
mutate "v080 alloc memory share"         internal/core/shared.go '		share = math.Max(share, float64(memMB)/float64(nodes)/nodeMem)' '		_ = nodeMem'
mutate "v080 interactive share"          internal/core/helpers.go '		share = cr.NodeShare
' '		_ = cr
'
mutate "v080 interactive no-cpu warning" internal/core/helpers.go '		if in.CPUs == 0 {
			r.Warnings' '		if false {
			r.Warnings'
mutate "v090 program tier ceiling"       internal/server/server.go '	c.Tiers = ceiling(c.Tiers, p.Tiers)' '	_ = ceiling'
mutate "v090 ceiling never grants"        internal/server/store.go '		for _, l := range limit {
			if t == l {' '		for _, l := range limit {
			if t == l || l != "" {'
mutate "v090 program budget"              internal/server/server.go '		c.Limits.CallsPerMin = p.CallsPerMin' '		_ = p'
mutate "v090 program conn is separate"    internal/server/server.go '	return email + "|" + p.ID' '	return email'
mutate "v090 disabled program per request" internal/server/server.go '		p, err := s.programFor(a.ClientID)
		if err != nil {
			s.audit("mcp", u.Email, "denied", err.Error())' '		p, err := s.programFor(a.ClientID)
		if false {
			s.audit("mcp", u.Email, "denied", err.Error())'
mutate "v090 disabled program refresh"    internal/server/oauth.go '	if _, err := s.programFor(rec.ClientID); err != nil {' '	if false {'
mutate "v090 disabled program sign-in"    internal/server/oauth.go '		p := s.users.Program(id)
		if p == nil {
			return nil, errors.New("client is disabled")
		}' '		p := s.users.Program(id)
		if p == nil {
			p = \&ProgramClient{ID: id, RedirectURIs: []string{"http://127.0.0.1:33418/callback"}}
		}'
mutate "v090 disabled program code exchange" internal/server/oauth.go '		if _, err := s.programFor(c.ClientID); err != nil {
			oauthErr(w, 400, "invalid_grant", err.Error())
			return
		}
		s.issue(w, c.ClientID, c.Email)' '		s.issue(w, c.ClientID, c.Email)'
mutate "v090 program redirect check"      internal/server/store.go '			if !validRedirect(r) {
				return fmt.Errorf("users file: client %s redirect' '			if false {
				return fmt.Errorf("users file: client %s redirect'
mutate "v090 program budget range"        internal/server/store.go '		if c.CallsPerMin < 0 || c.CallsPerMin > MaxProgramCallsPerMin {' '		if false {'
mutate "v090 one backend per person"      internal/server/server.go '	if b, ok := s.backends[email]; ok {
		return b
	}' ''
mutate "v090 audit names the program"     internal/core/service.go '		client = "program:" + s.Program + "/" + client' '		_ = client'
mutate "v091 private output never shared" internal/core/service.go '	public := c.Public() && !c.Write()' '	public := !c.Write()'
mutate "v091 per-person flight key"       internal/core/service.go '			fkey = "u:" + s.Principal + ":" + s.Program + ":" + key' '			fkey = key'
mutate "v091 shared entry freshness"      internal/core/shared_cache.go '	if ok && c.now().Sub(e.at) < ttl {' '	if ok {'
mutate "v091 errors not stored"           internal/core/shared_cache.go '	if store && f.err == nil {' '	if store {'
mutate "v091 writes never coalesced"      internal/core/service.go '	if s.Shared != nil && ttl > 0 && !c.Write() {' '	if s.Shared != nil && !c.Write() || s.Shared != nil && c.Write() {'
mutate "v092 in-flight cap enforced" internal/core/service.go '		if !s.flights.acquire(s.MaxInFlight) {' '		if false && !s.flights.acquire(s.MaxInFlight) {'
mutate "v092 in-flight slot released" internal/core/service.go '		defer s.flights.release()' '		_ = 0'
mutate "v092 in-flight cap exact" internal/core/inflight.go '	if f.n >= max {' '	if f.n > max {'
mutate "v092 accounting gated" internal/core/service.go '	if c.Kind() == backend.KindAcct && s.Gate != nil {' '	if false && s.Gate != nil {'
mutate "v092 accounting slot released" internal/core/inflight.go '		return func() { <-g.acct }, nil' '		return func() {}, nil'
mutate "v092 hosted server sets cap" internal/server/server.go '	svc.MaxInFlight = core.MaxInFlight' '	svc.MaxInFlight = 0'
mutate "v092 hosted server sets gate" internal/server/server.go '	svc.Gate = s.gate' '	svc.Gate = nil'
mutate "v093 job_ids filter applied" internal/core/jobs.go '			if want[strings.SplitN(j.JobID, "_", 2)[0]] {' '			if true {'
mutate "v093 job_ids validated" internal/core/jobs.go '		if err := backend.ValidJobID(id); err != nil {
			return nil, err
		}
		want[' '		if false {
			return nil, nil
		}
		want['
mutate "v093 job_ids count cap" internal/core/jobs.go '	if len(in.JobIDs) > MaxJobIDs {' '	if false {'
mutate "v093 restarts on queue rows" internal/core/jobs.go '		Restarts: j.RestartCnt.Int(),' '		Restarts: 0,'
mutate "v093 restarts on accounting rows" internal/core/jobs.go 'ExitCode: j.ExitCode.String(), Restarts: j.RestartCnt,' 'ExitCode: j.ExitCode.String(),'
mutate "v093 program own day cap" internal/server/server.go '		c.Caps.MaxCostPerDayUSD = p.MaxCostPerDayUSD' '		_ = p.MaxCostPerDayUSD'
mutate "v093 program own job cap" internal/server/server.go '			c.Caps.MaxCostPerJobUSD = p.MaxCostPerJobUSD' '			_ = p.MaxCostPerJobUSD'
mutate "v093 program own submits" internal/server/server.go '			c.Caps.MaxSubmitsPerDay = p.MaxSubmitsPerDay' '			_ = p.MaxSubmitsPerDay'
mutate "v093 program own ledger" internal/server/server.go '		c.StatePath = strings.TrimSuffix(c.StatePath, ".json") + "-" + p.ID + ".json"' '		_ = c.StatePath'
mutate "v093 program day cap bound" internal/server/store.go '		if c.MaxCostPerDayUSD < 0 || c.MaxCostPerDayUSD > MaxProgramDayUSD {' '		if false {'
mutate "v093 program job cap within day" internal/server/store.go '		if c.MaxCostPerJobUSD < 0 || (c.MaxCostPerJobUSD > 0 && c.MaxCostPerJobUSD > c.MaxCostPerDayUSD) {' '		if false {'
mutate "v093 program submits bound" internal/server/store.go '		if c.MaxSubmitsPerDay < 0 || c.MaxSubmitsPerDay > MaxProgramSubmitsDay {' '		if false {'
mutate "v093 program caps as a set" internal/server/store.go '		if c.MaxSubmitsPerDay > 0 && c.MaxCostPerDayUSD == 0 {' '		if false {'
mutate "v094 redirect ends module list" internal/core/cluster.go '		if strings.ContainsAny(w, "<>") {
			break' '		if false {
			break'
mutate "v095 extend not replace"   internal/backend/iap.go '		if time.Until(k.Expires) < RenewWindow {' '		if false {'
mutate "v095 keep slow new key"    internal/backend/iap.go '		// keep it: OS Login removes it at expiry, and the next call reuses it' '		_ = b.deleteKeyLine(context.Background(), line)'
mutate "v095 store before login"   internal/backend/iap.go '	_ = b.keys().Put(b.Email, nk)' '	_ = nk'
mutate "v095 wait for fresh key"   internal/backend/iap.go '				fresh := time.Since(k.Imported) < keyPropagation' '				fresh := false'
mutate "v095 gone fresh key replaced" internal/backend/iap.go '	return err != nil || code != 404' '	return true'
mutate "v095 fresh key retries"    internal/backend/iap.go '						client, err = b.handshake(ctx, k.User, signer, 13)' '						client, err = b.handshake(ctx, k.User, signer, 0)'
mutate "v096 env in a job"         internal/backend/files.go '	argv := append(EnvJobArgs(partition), "bash", "-lc", envTemplate, "bifrost", strconv.Itoa(len(modules)))' '	argv := []string{"bash", "-lc", envTemplate, "bifrost", strconv.Itoa(len(modules))}'
mutate "v096 env one core"         internal/backend/files.go '"-N", "1", "-n", "1", "-c", "1", "-t", "3",' '"-N", "1", "-n", "1", "-t", "3",'
mutate "v096 env gives up"         internal/backend/files.go '		"--immediate=120", "--quiet", "-J", "bifrost-env-check"}' '		"--quiet", "-J", "bifrost-env-check"}'
mutate "v096 env partition checked" internal/backend/files.go '	if !rePartition.MatchString(partition) {
		return Command{}, fmt.Errorf("env_check partition' '	if false {
		return Command{}, fmt.Errorf("env_check partition'
mutate "v096 env uses config"      internal/core/helpers.go '	c, err := backend.EnvCheck(s.Cfg.EnvPartition, modules, commands, versionCommands)' '	c, err := backend.EnvCheck("standard", modules, commands, versionCommands)'
mutate "v096 env reports job"      internal/core/helpers.go '			r.JobID, r.Node = policy.CleanLabel(f[1], 20), policy.CleanLabel(f[2], 80)' '			_ = f'
mutate "v096 env timeout"          internal/backend/files.go '	return Command{argv: argv, kind: KindNoCache, timeout: 200 * time.Second}, nil' '	return Command{argv: argv, kind: KindNoCache, timeout: 120 * time.Second}, nil'
mutate "v096 config partition"     internal/config/config.go '	if !rePartitionName.MatchString(c.EnvPartition) {' '	if false {'
mutate "v097 job_ids uncached"     internal/backend/command.go '	return Command{argv: []string{"squeue", "--json", "-j", list}, kind: KindNoCache, okExit: []int{1}}, nil' '	return Command{argv: []string{"squeue", "--json", "-j", list}, kind: KindQueue, okExit: []int{1}}, nil'
mutate "v097 job_ids by id"        internal/core/jobs.go '	byID := len(want) > 0 && !in.All' '	byID := false'
mutate "v097 own queue rows only"  internal/core/jobs.go '		if byID && j.UserName != who {' '		if false {'
mutate "v097 own acct rows only"   internal/core/jobs.go '		if byID && j.User != who {' '		if false {'
mutate "v097 job list validated"   internal/backend/command.go '		if err := ValidJobID(id); err != nil {
			return "", err' '		if false {
			return "", nil'
mutate "v098 idle alt order"       internal/core/a1.go '	altPartitions     = []string{"standard", "spot", "computehigh", "nvmescratch", "highmem"}' '	altPartitions     = []string{"computehigh", "standard", "spot", "nvmescratch", "highmem"}'
mutate "v098 cold alt order"       internal/core/a1.go '	altColdPartitions = []string{"standard", "spot", "computehigh"}' '	altColdPartitions = []string{"computehigh", "standard", "spot"}'
mutate "v099 time over MaxTime"     internal/core/cluster.go '		if maxMin, ok := slurmMinutes(p.TimeLimit); ok && hasTime && maxMin > 0 && mins > maxMin {' '		if maxMin, ok := slurmMinutes(p.TimeLimit); false && ok && hasTime && maxMin > 0 && mins > maxMin {'
mutate "v099 time unlimited ok"    internal/core/cluster.go '		if maxMin, ok := slurmMinutes(p.TimeLimit); ok && hasTime && maxMin > 0 && mins > maxMin {' '		if maxMin, ok := slurmMinutes(p.TimeLimit); hasTime && mins > maxMin && ok || p.TimeLimit == "infinite" {'
mutate "v099 avx partitions"       internal/core/cluster.go '		if avx2OnlyPartitions[part] {' '		if true {'
mutate "v099 avx comments"         internal/core/cluster.go '			if m := reAVX512.FindString(codeOnly(script)); m != "" {' '			if m := reAVX512.FindString(script); m != "" {'
mutate "v099 mpi hint multinode"   internal/core/cluster.go '		if part == "standard" && nodes > 1 && reMPIRun.MatchString(codeOnly(script)) {' '		if part == "standard" && reMPIRun.MatchString(codeOnly(script)) {'
mutate "v099 waste ch standard"    internal/core/p2.go '			if j.Partition == "computehigh" && cores <= 16 {' '			if j.Partition == "computehigh" && cores <= 0 {'
mutate "v099 waste ch only"        internal/core/p2.go '			if j.Partition == "computehigh" && cores <= 16 {' '			if cores <= 16 {'
mutate "v0910 sigill not crash"    internal/rules/rules.go '			if r.id == "tool-crash" && hasRule(out, "illegal-instruction") {' '			if false {'
mutate "v0910 sigill exit quiet"   internal/rules/rules.go '"tool-crash", "illegal-instruction", "oom"' '"tool-crash", "oom"'
mutate "v0910 cpu note"            internal/core/cluster.go '	if avx2OnlyPartitions[part] {
		return "AVX2 only (e2)"' '	if false {
		return "AVX2 only (e2)"'
mutate "v0911 db- lists all"     internal/core/catalog.go '		return append([]Dataset(nil), c.Datasets.Items...)' '		return nil'
mutate "v0911 storage db note"   internal/core/helpers.go 'len(cat.Datasets.Items) > 0 {' 'len(cat.Datasets.Items) > 9999 {'
mutate "v0912 db download warn"   internal/core/dbcheck.go '				if !ok || seen[name] {' '				if true {'
mutate "v0912 hosted only"        internal/core/dbcheck.go '				if !ok || seen[name] {' '				if seen[name] {'
mutate "v0912 array percent"      internal/core/dbcheck.go '	if m := reArraySpc.FindStringSubmatch(spec); m != nil {' '	if m := reArraySpc.FindStringSubmatch(spec); m != nil && false {'
mutate "v0912 big scan gate"      internal/core/dbcheck.go '	if n := arrayConcurrency(array); n > 8 && reBigScan.MatchString(codeOnly(script)) {' '	if n := arrayConcurrency(array); n > 8 {'
mutate "v095 background renew"     internal/backend/iap.go '		b.touch()
		b.renewSoon()' '		b.touch()'
mutate "v095 renew updates store"  internal/backend/iap.go '			k.Expires = exp
			_ = b.keys().Put(b.Email, k)
		}
	}()' '		}
	}()'
mutate "v0914 step rows not jobs"   internal/slurm/sacctrows.go '		if parent, _, isStep := strings.Cut(id, "."); isStep {' '		if parent, _, isStep := strings.Cut(id, "."); isStep && false {'
mutate "v0914 step peak memory"    internal/slurm/sacctrows.go '			jobs[i].Steps = append(jobs[i].Steps, st)' '			_ = st'
mutate "v0914 signal exit"         internal/slurm/sacctrows.go '	if n := atoi(sig); n != 0 {' '	if n := atoi(sig); n != 0 && false {'
mutate "v0914 never started nodes" internal/slurm/sacctrows.go '		if j.Nodes == "None assigned" {' '		if false {'
mutate "v0914 state before by"     internal/slurm/sacctrows.go '		state, _, _ := strings.Cut(f[3], " ") // "CANCELLED by 50001"' '		state := f[3]'
mutate "v0914 name keeps pipes"    internal/slurm/sacctrows.go '		f := strings.SplitN(line, "|", sacctFieldCount)' '		f := strings.Split(line, "|")'
mutate "v0914 cpu days"            internal/slurm/sacctrows.go '		days, s = atoi(d), rest' '		s = rest; _ = d'
mutate "v0914 mem MB"              internal/slurm/sacctrows.go '			t.Count = memBytes(v) / (1024 * 1024)' '			t.Count = memBytes(v)'
mutate "v0914 usage uses summary"  internal/core/cluster.go '		c, err = backend.SacctSummaryUser(firstNonEmpty(in.User, me), in.Since, in.Until)' '		c, err = backend.SacctUser(firstNonEmpty(in.User, me), in.Since, in.Until)'
mutate "v0914 jobs uses summary"   internal/core/jobs.go '			ac, err = backend.SacctSummaryUser(who, in.Since, "")' '			ac, err = backend.SacctUser(who, in.Since, "")'
mutate "v0914 user not widened"    internal/backend/command.go '	return sacctSummary([]string{"-u", user}, since, until)' '	return sacctSummary([]string{"-a"}, since, until)'
mutate "v0914 user validated"      internal/backend/command.go '	if err := ValidUser(user); err != nil {
		return Command{}, err
	}
	return sacctSummary(' '	if false {
		return Command{}, nil
	}
	return sacctSummary('
mutate "v0914 script note"         internal/core/jobs.go '				d.ScriptNote = "Slurm did not store' '				_ = "Slurm did not store'
mutate "v0914 du parallel"         internal/backend/files.go 'xargs -0 -r -P 8 -n 1' 'xargs -0 -r -P 1 -n 1'
mutate "v0914 du budget"           internal/backend/files.go 'end=$(( $(date +%s) + 40 ))' 'end=$(( $(date +%s) + 100 ))'
restore
for f in $FILES; do diff -q "$BK/$f" "$f" >/dev/null || { echo "NOT RESTORED: $f"; FAIL=1; }; done
exit $FAIL
