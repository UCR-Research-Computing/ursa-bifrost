#!/usr/bin/env bash
# Mutation check: disable each safety guard in turn and confirm a test fails.
# Restores every file from a backup copy in a trap. Run: scripts/mutation_check.sh
set -u
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.26.8} PYTHONDONTWRITEBYTECODE=1
BK=$(mktemp -d)
FILES="internal/policy/audit.go internal/core/jobs.go internal/backend/command.go internal/core/service.go internal/policy/redact.go internal/core/a1.go internal/core/results.go internal/slurm/types.go internal/core/cluster.go internal/backend/iap.go internal/rules/rules.go internal/server/oauth.go internal/server/server.go internal/server/store.go internal/server/google.go internal/core/files.go internal/core/staged.go internal/core/helpers.go internal/backend/files.go internal/staging/staging.go"
for f in $FILES; do mkdir -p "$BK/$(dirname "$f")"; cp "$f" "$BK/$f"; done
restore() { for f in $FILES; do cp "$BK/$f" "$f"; done; }
trap restore EXIT

mutate() { # name file python-replace(old,new)
  local name=$1 file=$2 old=$3 new=$4
  case " $FILES " in *" $file "*) ;; *) echo "ERROR $name: $file is not in FILES (would not be restored)"; exit 2;; esac
  python3 - "$file" "$old" "$new" <<'EOF'
import sys
p, old, new = sys.argv[1:]
t = open(p).read()
assert t.count(old) == 1, f"pattern not unique in {p}: {old!r}"
open(p, "w").write(t.replace(old, new))
EOF
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
mutate "iap reuse margin"     internal/backend/iap.go 'time.Until(k.Expires) > 10*time.Minute' 'true'
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
mutate "srv whoami auth"        internal/server/server.go '	_, u, err := s.verifyAccess(tok)
	if err != nil {
		s.challenge(w, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"email": u.Email, "tiers": u.Tiers})' '	u := s.users.Lookup("bob@ucr.edu")
	writeJSON(w, 200, map[string]any{"email": u.Email, "tiers": u.Tiers})'
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
restore
for f in $FILES; do diff -q "$BK/$f" "$f" >/dev/null || { echo "NOT RESTORED: $f"; FAIL=1; }; done
exit $FAIL
