#!/usr/bin/env python3
"""Onboard one lab onto the shared Ursa Major cluster through bifrost.

    scripts/onboard_lab.py plan  LAB.yaml   # read-only: shows every change (default)
    scripts/onboard_lab.py apply LAB.yaml   # backs up, applies, validates; asks first

LAB.yaml:
    id: chang                    # bifrost lab id (^[a-z0-9][a-z0-9-]{1,62}$)
    name: Chang Lab
    allocation_usd: 1000         # per month, list price, warn-only (SPEC 24)
    person_allocation_usd: 400   # optional default per person
    tiers: [R1, A1]              # default: read + submit/cancel own jobs
    people: [chiaenc@ucr.edu, ...]

What it changes (and nothing else):
  1. Project ucr-ursa-major-hpc-cluster IAM: roles/compute.osLogin and
     roles/iap.tunnelResourceAccessor for each person (what bifrost needs to reach the
     login node as them; same as Mike's access).
  2. Secret bifrost-users: a labs: entry and one users: line per person (existing lines
     for the same email are left untouched and reported).
  3. Rolls a new bifrost-mcp revision so the file is read.
It never notifies anyone. It never touches the lab's own project.
"""
import datetime, json, os, pathlib, re, subprocess, sys, tempfile

PROJECT = "ucr-ursa-major-hpc-cluster"
REGION = "us-central1"
SERVICE = "bifrost-mcp"
SECRET = "bifrost-users"
ROLES = ["roles/compute.osLogin", "roles/iap.tunnelResourceAccessor"]
LAB_ID = re.compile(r"^[a-z0-9][a-z0-9-]{1,62}$")
EMAIL = re.compile(r"^[a-z0-9._-]+@ucr\.edu$")


def sh(*args, input=None):
    return subprocess.run(args, check=True, capture_output=True, text=True, input=input).stdout


def load_yaml(text):
    import yaml  # run with: uv run --with pyyaml scripts/onboard_lab.py ...
    return yaml.safe_load(text)


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "plan"
    if mode not in ("plan", "apply") or len(sys.argv) < 3:
        sys.exit(__doc__)
    lab = load_yaml(pathlib.Path(sys.argv[2]).read_text())
    lid, people = lab["id"], [p.lower() for p in lab["people"]]
    if not LAB_ID.match(lid):
        sys.exit(f"bad lab id {lid!r}")
    bad = [p for p in people if not EMAIL.match(p)]
    if bad:
        sys.exit(f"not ucr.edu emails: {bad}")
    tiers = lab.get("tiers", ["R1", "A1"])
    pa = lab.get("person_allocation_usd")

    current = sh("gcloud", "secrets", "versions", "access", "latest", "--secret", SECRET, "--project", PROJECT)
    doc = load_yaml(current)
    have = {u["email"].lower() for u in doc.get("users", [])}
    labs = {l["id"] for l in doc.get("labs", []) or []}
    pol = json.loads(sh("gcloud", "projects", "get-iam-policy", PROJECT, "--format=json"))
    granted = {(b["role"], m) for b in pol["bindings"] for m in b["members"]}

    iam = [(r, p) for p in people for r in ROLES if (r, f"user:{p}") not in granted]
    new_users = [p for p in people if p not in have]
    skipped = [p for p in people if p in have]

    print(f"== Lab {lid} ({lab['name']}): ${lab['allocation_usd']}/month" + (f", ${pa}/person" if pa else ""))
    print(f"   lab entry: {'already present (left as is)' if lid in labs else 'add'}")
    print(f"== IAM on {PROJECT}: {len(iam)} grants")
    for r, p in iam:
        print(f"   + {p}  {r}")
    print(f"== users file: {len(new_users)} new lines, tiers {tiers}")
    for p in new_users:
        print(f"   + {p}")
    for p in skipped:
        print(f"   = {p} already in the file (not changed)")
    if mode == "plan":
        print("\nplan only: nothing changed")
        return

    if input("apply these changes? [y/N] ").strip() != "y":
        sys.exit("cancelled")
    stamp = datetime.datetime.now().strftime("%Y%m%d_%H%M%S")
    bk = pathlib.Path.home() / "backups" / f"bifrost-users_{stamp}_before_{lid}.yaml"
    bk.parent.mkdir(exist_ok=True)
    bk.write_text(current)
    bk.chmod(0o600)
    print(f"backup: {bk}")

    for r, p in iam:
        sh("gcloud", "projects", "add-iam-policy-binding", PROJECT, f"--member=user:{p}", f"--role={r}",
           "--condition=None", "--quiet", "--format=none")
        print(f"granted {r} to {p}")

    # text edit (keeps comments): lab block after "labs:" (or before "users:"), users appended under users:
    text = current
    if lid not in labs:
        block = (f"  - id: {lid}\n    name: {lab['name']}\n    allocation_usd: {lab['allocation_usd']}\n"
                 f"    period: month\n")
        if re.search(r"^labs:\n", text, re.M):
            text = re.sub(r"^labs:\n", "labs:\n" + block, text, count=1, flags=re.M)
        else:
            text = re.sub(r"^users:\n", "labs:\n" + block + "users:\n", text, count=1, flags=re.M)
    lines = "".join(
        f"  - email: {p}\n    tiers: [{', '.join(tiers)}]\n    lab: {lid}\n"
        + (f"    allocation_usd: {pa}\n" if pa else "")
        for p in new_users
    )
    if lines:
        text = re.sub(r"^users:\n", f"users:\n  # {lab['name']} onboarded {stamp[:8]}\n" + lines, text, count=1, flags=re.M)
    load_yaml(text)  # must still parse
    with tempfile.NamedTemporaryFile("w", delete=False, dir=os.environ.get("TMPDIR")) as f:
        f.write(text)
        tmp = f.name
    os.chmod(tmp, 0o600)
    try:
        print(sh("gcloud", "secrets", "versions", "add", SECRET, "--project", PROJECT, f"--data-file={tmp}",
                 "--format=value(name)").strip())
    finally:
        os.unlink(tmp)
    sh("gcloud", "run", "services", "update", SERVICE, "--project", PROJECT, "--region", REGION,
       f"--update-labels=users-rev={lid}-{stamp[:8]}", "--quiet")
    print("bifrost-mcp rolled; check: curl <url>/health and the revision log for 'users file' errors")


if __name__ == "__main__":
    main()
