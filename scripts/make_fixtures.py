#!/usr/bin/env python3
"""Make trimmed, anonymized test fixtures from a read-only probe of Ursa Major.

Real usernames, uids and internal IPs are replaced; batch scripts are dropped
except for one synthetic example. Input: /tmp/bifrost-probe/raw. Output: testdata/.
"""

import json
import re
import shutil
import sys
from pathlib import Path

RAW = Path(sys.argv[1] if len(sys.argv) > 1 else "/tmp/bifrost-probe/raw")
OUT = Path(__file__).resolve().parent.parent / "testdata"
# The real cluster user and uid are read from the probe itself (squeue meta),
# so no real identity is written into this public script.
FAKE = "alice_ucr_edu"
FAKE_UID = "50001"


def probe_identity() -> tuple[str, str]:
    q = json.loads((RAW / "squeue.json").read_text())
    user = q["meta"]["client"]["user"]
    uid = ""
    for j in q.get("jobs", []):
        if j.get("user_name") == user:
            uid = str(j.get("user_id", ""))
    if not uid:
        a = json.loads((RAW / "sacct_me.json").read_text())
        for j in a.get("jobs", []):
            for k in ("user_id", "uid"):
                if str(j.get(k, "")):
                    uid = str(j[k])
    return user, uid


USER, UID = probe_identity() if (RAW / "squeue.json").exists() else ("", "")

KEEP_JOBS = {37, 45, 195, 203, 212, 229, 236, 253, 93, 261, 41, 260}


def scrub(text: str) -> str:
    if USER:
        text = text.replace(USER, FAKE)
    if UID:
        text = text.replace(UID, FAKE_UID)
    text = re.sub(r"\b10\.\d+\.\d+\.\d+\b", "10.0.0.1", text)
    return text


def dump(name: str, obj) -> None:
    (OUT / name).write_text(scrub(json.dumps(obj, indent=1)) + "\n")


def main() -> None:
    OUT.mkdir(exist_ok=True)
    (OUT / "logs").mkdir(exist_ok=True)

    sacct = json.loads((RAW / "sacct_me.json").read_text())
    jobs = [j for j in sacct["jobs"] if j["job_id"] in KEEP_JOBS]
    for j in jobs:
        j["script"] = ""
        j["steps"] = j["steps"][:3]
    # one job keeps a small synthetic script so the include_script path is tested
    for j in jobs:
        if j["job_id"] == 236:
            j["script"] = (
                "#!/bin/bash\n#SBATCH -p gpul4\n#SBATCH -t 01:00:00\n"
                "export API_TOKEN=abc123secretvalue\npython run.py\n"
            )
    sacct["jobs"] = jobs
    dump("sacct_jobs.json", sacct)

    for name in ("sinfo.json", "squeue.json", "partitions.json", "catalog.json"):
        dump(name, json.loads((RAW / name).read_text()))

    nodes = json.loads((RAW / "nodes.json").read_text())
    nodes["nodes"] = nodes["nodes"][:6]
    dump("nodes.json", nodes)

    for f in sorted((RAW / "logs").glob("job_*.log")):
        if f.stat().st_size:
            (OUT / "logs" / f.name).write_text(scrub(f.read_text(errors="replace")))
    index = {}
    for j in jobs:
        log = OUT / "logs" / f"job_{j['job_id']}.log"
        if log.exists() and j.get("stdout_expanded"):
            index[scrub(j["stdout_expanded"])] = log.name
    (OUT / "logs" / "index.json").write_text(json.dumps(index, indent=1, sort_keys=True) + "\n")
    shutil.copy(RAW / "module_show.txt", OUT / "module_show.txt")
    (OUT / "module_show.txt").write_text(scrub((OUT / "module_show.txt").read_text()))
    leaks = [
        p
        for p in OUT.rglob("*")
        if p.is_file() and USER and (USER in p.read_text(errors="ignore"))
    ]
    if leaks:
        raise SystemExit(f"real username still present in {leaks}")
    print("fixtures written:", sorted(str(p.relative_to(OUT)) for p in OUT.rglob("*") if p.is_file()))


if __name__ == "__main__":
    main()
