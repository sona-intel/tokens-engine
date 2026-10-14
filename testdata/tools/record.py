#!/usr/bin/env python3
"""Records the real answers the tests read. Run from the repository root:

    python3 -I testdata/tools/record.py rpc|blockscout|gecko|prices|hypersync|lists|search|holdermap|arc|blockstats

rpc needs the RPC router on 127.0.0.1:4000, prices the prices service on 127.0.0.1:8210,
hypersync the key in ENVIO_HYPERSYNC_KEY (it is sent, never written). Every file holds the
response body exactly as the source sent it."""
import json
import os
import sys
import time
import urllib.error
import urllib.request

OUT = os.path.join(os.path.dirname(__file__), "..")
ROUTER = "http://127.0.0.1:4000/sona/"
MULTICALL3 = "ca11bde05977b3631167028862be2a173976ca11"
SEL = {"name": "06fdde03", "symbol": "95d89b41", "decimals": "313ce567", "totalSupply": "18160ddd"}
TRANSFER = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
DEPOSIT = "0xe1fffcc4923d04b559f4d29a8bfc6cda04eb5b0d3c460751c2402c5c5cc9109c"
WITHDRAWAL = "0x7fcf532c15f0a6db0bd6d0e038bea71d30d808c7d98cb3bf7268a95bf5081b65"
LOG_FIELDS = ["block_number", "log_index", "transaction_hash", "address", "data", "topic0", "topic1", "topic2"]

ETH_USDC = "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
ETH_MKR = "0x9f8f72aa9304c8b593d555f12ef6589cc3a579a2"
# an externally owned account: Blockscout lists it with is_contract false among USDC's holders
ETH_EOA = "0x37305b1cd40574e4c5ce33f8e8306be057fd7341"
MON_USDC = "0x754704bc059f8c67012fed69bc8a327a5aafb603"
MON_WMON = "0x3bd359c1119da7da1d913d1c4d2b7c461115433a"


def save(path, body):
    full = os.path.join(OUT, path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "wb") as f:
        f.write(body)
    print(path, len(body), "bytes")


def fetch(url, data=None, headers=None):
    # the engine's own user agent, Cloudflare in front of Blockscout refuses Python's
    req = urllib.request.Request(url, data=data, headers={"User-Agent": "sona-tokens-engine/1", **(headers or {})})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read(), dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)


def word(n):
    return format(n, "064x")


def aggregate3(calls):
    """aggregate3((address,bool,bytes)[]) calldata, calls are (target, allow, data hex)"""
    head = word(32) + word(len(calls))
    offsets, tails, at = [], [], 32 * len(calls)
    for target, allow, data in calls:
        offsets.append(word(at))
        padded = data + "0" * ((64 - len(data) % 64) % 64)
        tails.append(word(int(target, 16)) + word(1 if allow else 0) + word(96) + word(len(data) // 2) + padded)
        at += 128 + len(padded) // 2
    return "0x82ad56cb" + head + "".join(offsets) + "".join(tails)


def rpc(chain, method, params, request=None):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    if request:
        save(request, body)
    status, raw, hdr = fetch(ROUTER + chain, body, {"Content-Type": "application/json", "X-ERPC-Use-Upstream": "!drpc-*"})
    credits = hdr.get("X-Erpc-Credits", "")
    assert status == 200 and "drpc" not in credits.lower(), (status, credits)
    return raw


def meta(chain, token, name):
    calls = [(token, True, SEL[k]) for k in ("name", "symbol", "decimals", "totalSupply")]
    save(f"rpc/meta.{name}.json", rpc(chain, "eth_call", [{"to": "0x" + MULTICALL3, "data": aggregate3(calls)}, "latest"], f"rpc/meta.{name}.request.json"))


def record_rpc():
    meta("ethereum", ETH_USDC, "eth_usdc")
    meta("ethereum", ETH_MKR, "eth_mkr")
    meta("ethereum", ETH_EOA, "eth_eoa")
    meta("monad", MON_USDC, "monad_usdc")
    meta("monad", MON_WMON, "monad_wmon")
    holders = ["0000000000000000000000003bd359c1119da7da1d913d1c4d2b7c461115433a", "000000000000000000000000000000000000000000000000000000000000dead"]
    calls = [(MON_USDC, True, "70a08231" + h) for h in holders] + [(MON_USDC, True, "deadbeef")]
    save("rpc/balanceof.monad_usdc.json", rpc("monad", "eth_call", [{"to": "0x" + MULTICALL3, "data": aggregate3(calls)}, "latest"], "rpc/balanceof.monad_usdc.request.json"))
    # the first and last blocks of the recorded HyperSync pages, for the transfer windows
    for chain, n in [("monad", 1), ("monad", 113000000), ("monad", 113000039), ("ethereum", 26180000), ("ethereum", 26180999)]:
        save(f"rpc/header.{chain}_{n}.json", rpc(chain, "eth_getBlockByNumber", [hex(n), False]))


def record_blockscout():
    base = "https://eth.blockscout.com/api/v2/tokens/"
    for name, path in [("token.usdc", ETH_USDC), ("holders.usdc.page1", ETH_USDC + "/holders"), ("token.mkr", ETH_MKR), ("token.eoa", ETH_EOA)]:
        status, raw, _ = fetch(base + path, headers={"Accept": "application/json"})
        save(f"blockscout/{name}.json", raw)
        print(" status", status)
        if name == "holders.usdc.page1":
            nxt = json.loads(raw)["next_page_params"]
            q = "&".join(f"{k}={v}" for k, v in nxt.items())
            status, raw2, _ = fetch(base + path + "?" + q, headers={"Accept": "application/json"})
            save("blockscout/holders.usdc.page2.json", raw2)
        time.sleep(0.5)


def record_gecko():
    base = "https://api.geckoterminal.com/api/v2/networks/monad/tokens/"
    for name, path in [("info.usdc", MON_USDC + "/info"), ("token.usdc", MON_USDC), ("info.wmon", MON_WMON + "/info"),
                       ("info.unknown", "0x000000000000000000000000000000000000dead/info")]:
        status, raw, _ = fetch(base + path, headers={"Accept": "application/json"})
        save(f"gecko/{name}.json", raw)
        print(" status", status)
        time.sleep(2.5)


def record_prices():
    for name, url in [("monad_usdc", f"http://127.0.0.1:8210/internal/v1/chains/eip155:143/tokens/{MON_USDC}/prices?range=1d"),
                      ("unknown", "http://127.0.0.1:8210/internal/v1/chains/eip155:1/tokens/0x000000000000000000000000000000000000dead/prices?range=1d")]:
        status, raw, _ = fetch(url)
        save(f"prices/{name}.json", raw)
        print(" status", status)


def hypersync(host, name, query):
    key = os.environ["ENVIO_HYPERSYNC_KEY"]
    body = json.dumps(query).encode()
    for _ in range(12):
        status, raw, hdr = fetch(f"https://{host}/query", body, {"Authorization": "Bearer " + key, "Content-Type": "application/json"})
        if status != 429:
            break
        wait = int(hdr.get("x-ratelimit-reset", "10")) + 0.3
        print(" 429, waiting", wait)
        time.sleep(wait)
    assert status == 200, status
    save(f"hypersync/{name}.json", raw)
    save(f"hypersync/{name}.request.json", body)


LIST_URLS = {
    "uniswap": "https://tokens.uniswap.org",
    "monad": "https://raw.githubusercontent.com/monad-crypto/token-list/refs/heads/main/tokenlist-mainnet.json",
}
# the Uniswap list is too long to keep whole (700 KB), so it keeps its header and, as parsed and written again,
# the Ethereum and Monad entries whose symbol or name holds one of these words
LIST_WORDS = ("USDC", "WMON", "PEPE", "MON")
LIST_CHAINS = (1, 143)


def record_lists():
    for name, url in LIST_URLS.items():
        status, raw, _ = fetch(url, headers={"Accept": "application/json"})
        assert status == 200, (name, status)
        if name == "monad":
            save("lists/monad.json", raw)
            continue
        doc = json.loads(raw)
        doc["tokens"] = [t for t in doc["tokens"] if t["chainId"] in LIST_CHAINS and any(w in (t["symbol"] + " " + t["name"]).upper() for w in LIST_WORDS)]
        save(f"lists/{name}.json", json.dumps(doc, ensure_ascii=False, separators=(",", ":")).encode())
        time.sleep(1)


def record_search():
    base = "https://api.geckoterminal.com/api/v2/search/pools?include=base_token%2Cquote_token&network="
    for network in ("eth", "monad"):
        for q in ("usdc", "mon", "wmon", "pepe"):
            for _ in range(6):
                status, raw, _ = fetch(f"{base}{network}&query={q}", headers={"Accept": "application/json"})
                if status != 429:
                    break
                print(" 429, waiting")
                time.sleep(30)
            assert status == 200, status
            save(f"gecko/search.{network}.{q}.json", raw)
            print(" status", status)
            time.sleep(3)


def record_hypersync():
    fields = {"log": LOG_FIELDS, "block": ["number", "timestamp"]}
    hypersync("monad.hypersync.xyz", "monad_usdc", {"from_block": 113000000, "to_block": 113000040,
              "logs": [{"address": [MON_USDC], "topics": [[TRANSFER]]}], "field_selection": fields, "max_num_logs": 50000})
    hypersync("monad.hypersync.xyz", "monad_wmon", {"from_block": 113000000, "to_block": 113000300,
              "logs": [{"address": [MON_WMON], "topics": [[TRANSFER, DEPOSIT, WITHDRAWAL]]}], "field_selection": fields, "max_num_logs": 50000})
    hypersync("eth.hypersync.xyz", "eth_mkr", {"from_block": 26180000, "to_block": 26181000,
              "logs": [{"address": [ETH_MKR], "topics": [[TRANSFER]]}], "field_selection": fields, "max_num_logs": 50000})


# block 113,178,260 is the first Monad block of 2026-10-14 UTC, so the page holds 20 blocks of each day
MON_MIDNIGHT = 113178260


def record_blockstats():
    """The block query of Monad's chain activity: every block with its gas, and every transaction's block, sender and gas price"""
    hypersync("monad.hypersync.xyz", "monad_blocks", {"from_block": MON_MIDNIGHT - 20, "to_block": MON_MIDNIGHT + 20,
              "include_all_blocks": True, "transactions": [{}],
              "field_selection": {"block": ["number", "timestamp", "gas_used", "gas_limit"], "transaction": ["block_number", "from", "gas_price"]},
              "max_num_transactions": 50000})


ETH_UNI = "0x1f9840a85d5af5bf1d1762f925bdaddc4201f984"
# about 30 days of 12-second Ethereum slots
MAP_WINDOW_BLOCKS = 216000
MAP_PAGES = 8


def record_holdermap():
    """UNI's contract read, its first two Blockscout holder pages (100 holders), and the HyperSync pages of the
    holder map query: UNI Transfer logs whose sender and receiver are both among those 100, over the last
    216,000 blocks before the archive height, in at most 8 pages"""
    meta("ethereum", ETH_UNI, "eth_uni")
    base = "https://eth.blockscout.com/api/v2/tokens/" + ETH_UNI + "/holders"
    status, raw, _ = fetch(base, headers={"Accept": "application/json"})
    assert status == 200, status
    save("blockscout/holders.uni.page1.json", raw)
    page1 = json.loads(raw)
    q = "&".join(f"{k}={v}" for k, v in page1["next_page_params"].items())
    time.sleep(0.5)
    status, raw2, _ = fetch(base + "?" + q, headers={"Accept": "application/json"})
    assert status == 200, status
    save("blockscout/holders.uni.page2.json", raw2)
    holders = [it["address"]["hash"].lower() for it in page1["items"] + json.loads(raw2)["items"]]
    topics = ["0x" + "0" * 24 + h[2:] for h in holders]

    key = os.environ["ENVIO_HYPERSYNC_KEY"]
    status, raw, _ = fetch("https://eth.hypersync.xyz/height", headers={"Authorization": "Bearer " + key})
    assert status == 200, status
    head = json.loads(raw)["height"]
    start, end = head + 1 - MAP_WINDOW_BLOCKS, head + 1
    print(" head", head, "from", start)
    nxt = start
    for n in range(1, MAP_PAGES + 1):
        hypersync("eth.hypersync.xyz", f"eth_uni_map.p{n}", {"from_block": nxt, "to_block": end,
                  "logs": [{"address": [ETH_UNI], "topics": [[TRANSFER], topics, topics]}],
                  "field_selection": {"log": LOG_FIELDS}, "max_num_logs": 50000})
        with open(os.path.join(OUT, f"hypersync/eth_uni_map.p{n}.json")) as f:
            nxt = json.load(f)["next_block"]
        print(" next_block", nxt)
        if nxt >= end:
            break
    for b in (start, min(nxt, end) - 1):
        save(f"rpc/header.ethereum_{b}.json", rpc("ethereum", "eth_getBlockByNumber", [hex(b), False]))


# Circle's public endpoint, Sona's router had no Arc network when these were recorded
ARC_RPC = "https://rpc.mainnet.arc.io"
ARC_USDC = "0x3600000000000000000000000000000000000000"
# logs a Transfer at 18 decimals for every move of native USDC, the ERC-20 view logs a second one only for ERC-20 calls
ARC_EMITTER = "0xfffffffffffffffffffffffffffffffffffffffe"
ARC_EURC = "0xbef5f6d51cb62b58e6a8f77868681825c6fe21c1"
ARC_FROM, ARC_TO = 25941000, 25941019


def arc_rpc(method, params, request=None):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    if request:
        save(request, body)
    status, raw, _ = fetch(ARC_RPC, body, {"Content-Type": "application/json"})
    assert status == 200 and b'"error"' not in raw, (status, raw[:200])
    return raw


def arc_holders(logs):
    """the holders the index has after the recorded logs: the system emitter's moves cut to 6 decimals, positive
    balances only, largest first and ties by address"""
    deltas = {}
    for log in logs:
        if log["address"].lower() != ARC_EMITTER or len(log["topics"]) < 3:
            continue
        amount = int(log["data"], 16) // 10**12
        for topic, sign in ((log["topics"][1], -1), (log["topics"][2], 1)):
            who = "0x" + topic[-40:].lower()
            if amount and who != "0x" + "0" * 40:
                deltas[who] = deltas.get(who, 0) + sign * amount
    return [h for h, v in sorted(deltas.items(), key=lambda kv: (-kv[1], kv[0])) if v > 0]


def record_arc():
    for token, name in ((ARC_USDC, "arc_usdc"), (ARC_EURC, "arc_eurc")):
        calls = [(token, True, SEL[k]) for k in ("name", "symbol", "decimals", "totalSupply")]
        save(f"rpc/meta.{name}.json", arc_rpc("eth_call", [{"to": "0x" + MULTICALL3, "data": aggregate3(calls)}, "latest"], f"rpc/meta.{name}.request.json"))
    raw = arc_rpc("eth_getLogs", [{"fromBlock": hex(ARC_FROM), "toBlock": hex(ARC_TO), "address": [ARC_EMITTER, ARC_USDC, ARC_EURC], "topics": [[TRANSFER]]}],
                  "rpc/logs.arc_usdc.request.json")
    save("rpc/logs.arc_usdc.json", raw)
    holders = arc_holders(json.loads(raw)["result"])
    print(" holders", len(holders))
    at = hex(ARC_TO)
    native = [("0x" + MULTICALL3, True, "4d2301cc" + word(int(h, 16))) for h in holders]
    save("rpc/ethbalance.arc_usdc.json", arc_rpc("eth_call", [{"to": "0x" + MULTICALL3, "data": aggregate3(native)}, at], "rpc/ethbalance.arc_usdc.request.json"))
    erc20 = [(ARC_USDC, True, "70a08231" + word(int(h, 16))) for h in holders]
    save("rpc/balanceof.arc_usdc.json", arc_rpc("eth_call", [{"to": "0x" + MULTICALL3, "data": aggregate3(erc20)}, at], "rpc/balanceof.arc_usdc.request.json"))
    batch = json.dumps([{"jsonrpc": "2.0", "id": i, "method": "eth_getBalance", "params": [h, at]} for i, h in enumerate(holders)]).encode()
    save("rpc/getbalance.arc_usdc.request.json", batch)
    status, raw, _ = fetch(ARC_RPC, batch, {"Content-Type": "application/json"})
    assert status == 200 and b'"error"' not in raw, status
    save("rpc/getbalance.arc_usdc.json", raw)
    for n in (ARC_FROM, ARC_TO):
        save(f"rpc/header.arc_{n}.json", arc_rpc("eth_getBlockByNumber", [hex(n), False]))
    base = "https://api.geckoterminal.com/api/v2/networks/arc/tokens/"
    for name, path in (("info.arc_usdc", ARC_USDC + "/info"), ("token.arc_usdc", ARC_USDC)):
        status, raw, _ = fetch(base + path, headers={"Accept": "application/json"})
        assert status == 200, status
        save(f"gecko/{name}.json", raw)
        time.sleep(2.5)


if __name__ == "__main__":
    {"rpc": record_rpc, "blockscout": record_blockscout, "gecko": record_gecko, "prices": record_prices, "hypersync": record_hypersync, "lists": record_lists, "search": record_search,
     "holdermap": record_holdermap, "arc": record_arc, "blockstats": record_blockstats}[sys.argv[1]]()
