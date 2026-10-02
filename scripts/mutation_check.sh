#!/usr/bin/env bash
# Mutation check: disable each safety guard in turn and confirm a test fails.
# Restores every file from a backup copy in a trap. Run: scripts/mutation_check.sh
set -u
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.26.8} PYTHONDONTWRITEBYTECODE=1
BK=$(mktemp -d)
FILES="internal/policy/audit.go internal/core/jobs.go internal/backend/command.go internal/core/service.go internal/policy/redact.go internal/core/a1.go internal/core/results.go internal/slurm/types.go internal/core/cluster.go internal/backend/iap.go internal/server/oauth.go internal/server/server.go internal/server/store.go internal/server/google.go"
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
mutate "A1 plan hash check"   internal/core/a1.go 'hashOf(p.Script)) != p.Hash {' 'hashOf(p.Script)) == "never" {'
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
restore
for f in $FILES; do diff -q "$BK/$f" "$f" >/dev/null || { echo "NOT RESTORED: $f"; FAIL=1; }; done
exit $FAIL
