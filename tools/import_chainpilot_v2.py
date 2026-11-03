"""Copy the original catalog metadata and missing exits without changing ChainPilot."""
from pathlib import Path
from datetime import datetime, timezone
import argparse
import json
import os
import sqlite3


def connection_proxy(connection):
    result = dict(connection.get("Options") or {})
    result.update(type=str(connection["Protocol"]).lower(), server=connection["Server"], port=connection["Port"])
    for original, target in (("Username", "username"), ("Password", "password"), ("SNI", "sni"), ("Fingerprint", "client-fingerprint")):
        if connection.get(original):
            result[target] = connection[original]
    if connection.get("SkipCertVerify"):
        result["skip-cert-verify"] = True
    return result


def migrate(source, root):
    path = root / "data" / "config.json"
    marker = root / "data" / "chainpilot-v2-import.json"
    if marker.exists():
        print('{"ok":true,"alreadyImported":true}')
        return
    cfg = json.loads(path.read_text(encoding="utf-8"))
    db = sqlite3.connect((source / "chainpilot.db").as_uri() + "?mode=ro", uri=True)
    try:
        app = json.loads(db.execute("select document_json from documents where namespace=?", ("chainpilot.application.v1",)).fetchone()[0])
        state = json.loads(db.execute("select state_json from chain_manager_state").fetchone()[0])
    finally:
        db.close()
    backup = root / "data" / "backups" / "before-catalog-import-v2.json"
    backup.parent.mkdir(parents=True, exist_ok=True)
    backup.write_bytes(path.read_bytes())
    if not cfg.get("profiles") and cfg.get("fixedExit"):
        cfg["profiles"] = [dict(id="default", name=cfg["name"], expectedIP=cfg.get("expectedIP", ""), fixedExit=cfg["fixedExit"], proxyPort=cfg["proxyPort"], verifyPort=cfg["verifyPort"], controllerPort=cfg["controllerPort"], running=True, allowedSources=[])]
        cfg["fixedExit"] = None
        cfg["expectedIP"] = ""
    profiles = cfg.setdefault("profiles", [])
    sources = cfg.setdefault("sources", [])
    now = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    for src in app["sources"].values():
        if not any(s["id"] == src["ID"] for s in sources):
            sources.append(dict(id=src["ID"], name=src["Name"], url=src.get("URL", ""), enabled=src["Enabled"], priority=100, userAgent="Clash.Meta", updatedAt=now, lastError=""))
    relays = list(app["catalog"]["Relays"].values())
    assigned = 0
    for n in cfg["nodes"]:
        if n.get("sourceID") not in (None, "", "migrated"):
            continue
        p = n["proxy"]
        matches = [r for r in relays if str(r["Connection"]["Protocol"]).lower() == p["type"] and r["Connection"]["Server"] == p["server"] and str(r["Connection"]["Port"]) == str(p["port"])]
        if len(matches) == 1 and matches[0].get("SourceIDs"):
            n["sourceID"] = matches[0]["SourceIDs"][0]
            assigned += 1
        else:
            n["sourceID"] = "migrated"
        n.setdefault("priority", 100)
    if any(n["sourceID"] == "migrated" for n in cfg["nodes"]) and not any(s["id"] == "migrated" for s in sources):
        sources.append(dict(id="migrated", name="原 ChainPilot 中转", url="", enabled=True, priority=100, updatedAt=now))
    added = 0
    for original in state["Profiles"].values():
        p = connection_proxy(original["FixedExit"])
        if any(x["fixedExit"].get("type") == p["type"] and x["fixedExit"].get("server") == p["server"] and int(x["fixedExit"].get("port", 0)) == int(p["port"]) for x in profiles):
            continue
        occupied = {cfg["uiPort"]} | {port for x in profiles for port in (x["proxyPort"], x["verifyPort"], x["controllerPort"])}
        port = 27894
        while port in occupied:
            port += 2
        internal = 31000
        while internal in occupied or internal + 1 in occupied or port in (internal, internal + 1):
            internal += 2
        profiles.append(dict(id="imported-" + original["ID"], name=original["Name"], fixedExit=p, expectedIP=original.get("ExpectedEgressIP", ""), proxyPort=port, verifyPort=internal, controllerPort=internal + 1, running=False, allowedSources=[]))
        added += 1
    cfg.update(version=2, autoRefresh=True, refreshMinutes=cfg.get("refreshMinutes", 360), resumeOnLaunch=True)
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(cfg, ensure_ascii=False, indent=2), encoding="utf-8")
    os.replace(tmp, path)
    summary = dict(ok=True, addedProfiles=added, assignedNodes=assigned, sources=len(sources), profiles=len(profiles), nodes=len(cfg["nodes"]))
    marker.write_text(json.dumps(summary, ensure_ascii=False), encoding="utf-8")
    print(json.dumps(summary))


if __name__ == "__main__":
    args = argparse.ArgumentParser(description=__doc__)
    args.add_argument("--source", type=Path, default=Path(os.environ["LOCALAPPDATA"]) / "ChainPilot")
    args.add_argument("--root", type=Path, default=Path(__file__).resolve().parent.parent)
    opts = args.parse_args()
    try:
        migrate(opts.source, opts.root)
    except Exception as error:
        print(json.dumps({"ok": False, "errorType": type(error).__name__}))
        raise SystemExit(1)
