#!/usr/bin/env python3
"""Records the real answers the tests read. Run from the repository root:

    python3 -I testdata/tools/record.py rpc|blockscout|gecko|prices|hypersync|lists|search

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


if __name__ == "__main__":
    {"rpc": record_rpc, "blockscout": record_blockscout, "gecko": record_gecko, "prices": record_prices, "hypersync": record_hypersync, "lists": record_lists, "search": record_search}[sys.argv[1]]()
