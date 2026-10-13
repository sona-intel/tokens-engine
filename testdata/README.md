# Recorded answers

Every file here is a real answer, saved as the source sent it, on 2026-10-13 by `tools/record.py` (run from the repository root with `python3 -I testdata/tools/record.py <group>`). Nothing is edited or made up. Tests that need a page boundary or a failure build it around these answers, never inside them.

| Folder | Source | How |
|---|---|---|
| `rpc/` | Sona's RPC router on `127.0.0.1:4000`, free upstreams only (`X-ERPC-Use-Upstream: !drpc-*`, the recorder checks that no credit came from dRPC) | `meta.<chain>_<token>.json`: the `aggregate3` call of `name`, `symbol`, `decimals`, `totalSupply` for USDC and MKR (a bytes32 name and symbol) on Ethereum, USDC and WMON on Monad, and `0x37305b1cd40574e4c5ce33f8e8306be057fd7341`, an account without code that Blockscout lists with `is_contract: false`. Each has its request beside it (`.request.json`), built by the recorder's own Python encoder. `balanceof.monad_usdc.json`: two `balanceOf` calls and one call to an unknown selector. `header.<chain>_<block>.json`: `eth_getBlockByNumber` for the blocks that start and end the recorded HyperSync pages |
| `blockscout/` | `https://eth.blockscout.com/api/v2`, keyless, with the engine's user agent (Cloudflare refuses Python's) | `token.usdc.json`, `token.mkr.json`, `token.eoa.json` (a 404), `holders.usdc.page1.json` and `holders.usdc.page2.json` (the second page from the first page's `next_page_params`) |
| `gecko/` | `https://api.geckoterminal.com/api/v2/networks/monad`, keyless | `info.usdc.json`, `token.usdc.json`, `info.wmon.json` (no distribution), `info.unknown.json` (the 404 body for `0x…dead`) |
| `hypersync/` | `https://{monad,eth}.hypersync.xyz/query` with the key from `services/.env` (sent, never written) | `monad_usdc.json`: USDC transfers in blocks 113,000,000 to 113,000,039. `monad_wmon.json`: WMON transfers, deposits and withdrawals in blocks 113,000,000 to 113,000,299. `eth_mkr.json`: MKR transfers in Ethereum blocks 26,180,000 to 26,180,999. Each `.request.json` is the query, which the engine's own query body must equal |
| `prices/` | Sona's prices service on `127.0.0.1:8210` | `monad_usdc.json` (the 1-day chart of Monad USDC) and `unknown.json` (the 404 for `0x…dead` on Ethereum) |

The values in the tests (holder counts, top 10 shares, block times) are read from these files, so a new recording needs the expectations updated with it.
