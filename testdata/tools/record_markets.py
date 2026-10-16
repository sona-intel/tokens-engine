#!/usr/bin/env python3
"""Records the answers the market tests read. Run from the repository root:

    python3 -I testdata/tools/record_markets.py

Every file is a real answer, trimmed only by dropping rows or unused heavy fields (see testdata/README.md).
CoinGecko is read at most once every 21 seconds."""
import json
import os
import time
import urllib.request

OUT = os.path.join(os.path.dirname(__file__), "..")
DAY = 86400


def get(url):
    req = urllib.request.Request(url, headers={"User-Agent": "sona-tokens-engine/1", "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.loads(r.read())


def save(path, obj):
    full = os.path.join(OUT, path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "w") as f:
        json.dump(obj, f, separators=(",", ":"))
    print(path, os.path.getsize(full), "bytes")


def coingecko():
    markets = get("https://api.coingecko.com/api/v3/coins/markets?vs_currency=usd&order=market_cap_desc&per_page=100&page=1&price_change_percentage=24h")[:20]
    save("coingecko/markets.json", markets)
    time.sleep(21)
    coins = get("https://api.coingecko.com/api/v3/coins/list?include_platform=true")
    keep = {m["id"] for m in markets} | {"wrapped-monad", "cuberium"}
    save("coingecko/coins_list.json", [c for c in coins if c["id"] in keep])


HEAVY = {"description", "dimensions", "methodology", "hallmarks", "audit_links", "oraclesBreakdown", "tokenBreakdowns", "warningBanners", "tvlCodePath"}


def llama():
    for chain in ("Monad", "Ethereum"):
        tvl = get("https://api.llama.fi/v2/historicalChainTvl/" + chain)
        save("llama/tvl.%s.json" % chain.lower(), tvl[-400:])
        chart = get("https://stablecoins.llama.fi/stablecoincharts/" + chain)
        save("llama/stablecharts.%s.json" % chain.lower(), [{"date": c["date"], "totalCirculatingUSD": {"peggedUSD": c["totalCirculatingUSD"].get("peggedUSD")}} for c in chart[-400:]])
        dex = get("https://api.llama.fi/overview/dexs/%s?excludeTotalDataChart=false&excludeTotalDataChartBreakdown=true" % chain)
        dex.pop("allChains", None)
        dex["totalDataChart"] = dex["totalDataChart"][-400:]
        dex["protocols"] = [{k: v for k, v in p.items() if k not in HEAVY} for p in sorted(dex["protocols"], key=lambda p: -(p.get("total24h") or 0))[:20]]
        save("llama/dex.%s.json" % chain.lower(), dex)
    chains = get("https://api.llama.fi/v2/chains")
    save("llama/chains.json", [c for c in chains if c["name"] in ("Monad", "Ethereum")])
    protocols = get("https://api.llama.fi/protocols")
    rows = []
    for chain, n in (("Monad", 12), ("Ethereum", 12)):
        on = sorted((p for p in protocols if chain in (p.get("chainTvls") or {})), key=lambda p: -p["chainTvls"][chain])
        rows += on[:n]
        rows += [p for p in on if p["category"] in ("CEX", "Chain")][:2]
    seen, kept = set(), []
    for p in rows:
        if p["slug"] not in seen:
            seen.add(p["slug"])
            kept.append({k: v for k, v in p.items() if k not in HEAVY})
    save("llama/protocols.json", kept)
    stable = get("https://stablecoins.llama.fi/stablecoins?includePrices=true")
    assets = stable["peggedAssets"]
    on = lambda a, c: (a.get("chainCirculating") or {}).get(c, {}).get("current", {})
    pick = sorted((a for a in assets if on(a, "Monad")), key=lambda a: -(on(a, "Monad").get("peggedUSD") or 0))[:14]
    pick += sorted((a for a in assets if on(a, "Ethereum") and a not in pick), key=lambda a: -(on(a, "Ethereum").get("peggedUSD") or 0))[:8]
    pick += [a for a in assets if a["pegType"] == "peggedEUR" and on(a, "Ethereum") and a not in pick][:1]
    out = []
    for a in pick:
        a = {k: v for k, v in a.items() if k != "chains"}
        a["chainCirculating"] = {c: v for c, v in a["chainCirculating"].items() if c in ("Monad", "Ethereum")}
        out.append(a)
    save("llama/stablecoins.json", {"peggedAssets": out})
    arc = get("https://stablecoins.llama.fi/stablecoincharts/Arc")
    save("llama/stablecharts.arc.json", [{"date": c["date"], "totalCirculatingUSD": {"peggedUSD": c["totalCirculatingUSD"].get("peggedUSD")}} for c in arc[-400:]])
    save("llama/stablecoins.arc.json", {"peggedAssets": [dict(a, chainCirculating={"Arc": a["chainCirculating"]["Arc"]}) for a in assets if "Arc" in a["chainCirculating"]]})


def stats():
    today = time.gmtime()
    end = time.strftime("%Y-%m-%d", today)
    start = time.strftime("%Y-%m-%d", time.gmtime(time.time() - 120 * DAY))
    for line in ("newTxns", "activeAccounts", "newAccounts", "averageGasPrice", "txnsFee", "networkUtilization"):
        save("blockscout/stats.%s.json" % line, get("https://eth.blockscout.com/stats-service/api/v1/lines/%s?from=%s&to=%s" % (line, start, end)))
        time.sleep(1)


if __name__ == "__main__":
    import sys
    for step in (sys.argv[1:] or ["llama", "stats", "coingecko"]):
        globals()[step]()
