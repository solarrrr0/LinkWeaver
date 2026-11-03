"""Read-only ChainPilot migration. Never prints endpoints or credentials."""
from pathlib import Path
import argparse
import hashlib
import json
import os
import sqlite3
import urllib.request
import yaml

def migrate(source: Path, root: Path, overwrite: bool = False):
    path = root / "data" / "config.json"
    if path.exists() and not overwrite:
        raise ValueError("已有个人配置；如需重新导入，请显式使用 --overwrite。")
    with (source / "mihomo" / "active.yaml").open(encoding="utf-8") as f:
        active = yaml.safe_load(f)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open("http://127.0.0.1:8180/api/v1/workspace", timeout=5) as r:
        workspace = json.load(r)
    profiles = [p for p in workspace["profiles"] if p.get("running")]
    if len(profiles) != 1:
        raise ValueError("需要恰好一个运行中的 ChainPilot 出口以避免迁移错误。")
    profile = profiles[0]
    listener = next(x for x in active["listeners"] if x["port"] == profile["localProxyPort"])
    group = next(x for x in active["proxy-groups"] if x["name"] == listener["proxy"])
    by = {p["name"]: p for p in active["proxies"]}
    dbpath = source / "chainpilot.db"
    connection = sqlite3.connect(dbpath.as_uri() + "?mode=ro", uri=True)
    try:
        row = connection.execute("select document_json from documents where namespace=?", ("chainpilot.application.v1",)).fetchone()
        catalog = json.loads(row[0])["catalog"]["Relays"]
        catalog = list(catalog.values()) if isinstance(catalog, dict) else catalog
    finally:
        connection.close()
    views = {r["id"]: r for r in workspace["relays"]}
    chains = [by[n] for n in group["proxies"] if n in by and by[n].get("dialer-proxy")]
    if not chains:
        raise ValueError("没有发现完整中转到固定出口的链路。")
    fixed = {k: v for k, v in chains[0].items() if k not in ("name", "dialer-proxy")}
    if any({k: v for k, v in x.items() if k not in ("name", "dialer-proxy")} != fixed for x in chains):
        raise ValueError("候选组包含多个不同出口，暂不自动迁移。")
    nodes, seen = [], set()
    for chain in chains:
        raw = by[chain["dialer-proxy"]]
        proxy = {k: v for k, v in raw.items() if k != "name"}
        if "dialer-proxy" in proxy:
            raise ValueError("中转本身仍包含其他链路，需人工展开。")
        node_id = hashlib.sha256(json.dumps(proxy, sort_keys=True, ensure_ascii=False).encode()).hexdigest()[:16]
        if node_id in seen:
            continue
        seen.add(node_id)
        match = next((x for x in workspace["relays"] if str(x["id"]) in raw["name"]), None)
        if match is None:
            candidates = [x for x in catalog if str(x["Connection"].get("Protocol", "")).lower() == str(proxy["type"]).lower()
                          and x["Connection"].get("Server") == proxy["server"]
                          and str(x["Connection"].get("Port")) == str(proxy["port"])]
            if len(candidates) == 1:
                match = views.get(candidates[0]["ID"])
        label = match["name"] if match else f"线路 {len(nodes)+1:02d} · {str(proxy['type']).upper()}"
        nodes.append({"id": node_id, "label": label, "proxy": proxy, "seedAvailable": bool(match and match.get("health") == "available")})
    result = dict(version=1, name=profile["name"], expectedIP=profile.get("expectedEgressIp", ""), fixedExit=fixed,
                  nodes=nodes, targets=["https://www.gstatic.com/generate_204", "https://cp.cloudflare.com/generate_204"],
                  intervalSeconds=45, timeoutMS=6000, concurrency=3, minGainMS=60, minGainPercent=15, minDwellSeconds=120,
                  uiPort=28180, proxyPort=27892, verifyPort=27893, controllerPort=28191)
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        (path.parent / "config.previous.json").write_bytes(path.read_bytes())
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    os.replace(tmp, path)
    print(json.dumps({"ok": True, "nodes": len(nodes), "seedAvailable": sum(n["seedAvailable"] for n in nodes),
                      "expectedIPConfigured": bool(result["expectedIP"]), "output": str(path)}, ensure_ascii=False))

if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--source", type=Path, default=Path(os.environ["LOCALAPPDATA"]) / "ChainPilot")
    p.add_argument("--root", type=Path, default=Path(__file__).resolve().parent.parent)
    p.add_argument("--overwrite", action="store_true")
    a = p.parse_args()
    try:
        migrate(a.source, a.root, a.overwrite)
    except Exception as e:
        print("迁移失败：" + (str(e) if isinstance(e, ValueError) else type(e).__name__))
        raise SystemExit(1)
