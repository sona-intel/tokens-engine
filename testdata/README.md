# Recorded answers

Every file here is a real answer, saved as the source sent it, on 2026-10-13 by `tools/record.py` (run from the repository root with `python3 -I testdata/tools/record.py <group>`). Nothing is edited or made up. Tests that need a page boundary or a failure build it around these answers, never inside them.

| Folder | Source | How |
|---|---|---|
| `rpc/` | Sona's RPC router on `127.0.0.1:4000`, free upstreams only (`X-ERPC-Use-Upstream: !drpc-*`, the recorder checks that no credit came from dRPC) | `meta.<chain>_<token>.json`: the `aggregate3` call of `name`, `symbol`, `decimals`, `totalSupply` for USDC and MKR (a bytes32 name and symbol) on Ethereum, USDC and WMON on Monad, and `0x37305b1cd40574e4c5ce33f8e8306be057fd7341`, an account without code that Blockscout lists with `is_contract: false`. Each has its request beside it (`.request.json`), built by the recorder's own Python encoder. `balanceof.monad_usdc.json`: two `balanceOf` calls and one call to an unknown selector. `header.<chain>_<block>.json`: `eth_getBlockByNumber` for the blocks that start and end the recorded HyperSync pages |
| `blockscout/` | `https://eth.blockscout.com/api/v2`, keyless, with the engine's user agent (Cloudflare refuses Python's) | `token.usdc.json`, `token.mkr.json`, `token.eoa.json` (a 404), `holders.usdc.page1.json` and `holders.usdc.page2.json` (the second page from the first page's `next_page_params`) |
| `gecko/` | `https://api.geckoterminal.com/api/v2/networks/monad`, keyless | `info.usdc.json`, `token.usdc.json`, `info.wmon.json` (no distribution), `info.unknown.json` (the 404 body for `0x…dead`) |
| `hypersync/` | `https://{monad,eth}.hypersync.xyz/query` with the key from `services/.env` (sent, never written) | `monad_usdc.json`: USDC transfers in blocks 113,000,000 to 113,000,039. `monad_wmon.json`: WMON transfers, deposits and withdrawals in blocks 113,000,000 to 113,000,299. `eth_mkr.json`: MKR transfers in Ethereum blocks 26,180,000 to 26,180,999. Each `.request.json` is the query, which the engine's own query body must equal |
| `gecko/search.<network>.<q>.json` | `https://api.geckoterminal.com/api/v2/search/pools?include=base_token,quote_token&network=<network>&query=<q>` with network eth or monad, keyless | `usdc`, `mon`, `wmon` and `pepe` on both networks (`python3 -I testdata/tools/record.py search`) |
| `lists/` | the curated token lists, keyless | `monad.json`: the Monad Foundation's mainnet list as sent. `uniswap.json`: the Uniswap default list from `tokens.uniswap.org` reduced by the recorder to its header and the Ethereum and Monad entries whose symbol or name holds USDC, WMON, PEPE or MON, because the whole list is 700 KB (`python3 -I testdata/tools/record.py lists`) |
| `prices/` | Sona's prices service on `127.0.0.1:8210` | `monad_usdc.json` (the 1-day chart of Monad USDC) and `unknown.json` (the 404 for `0x…dead` on Ethereum) |

The values in the tests (holder counts, top 10 shares, block times) are read from these files, so a new recording needs the expectations updated with it.

## Markets

Recorded on 2026-10-13 by `python3 -I testdata/tools/record_markets.py [llama|stats|coingecko]`. Each file is a real answer, trimmed only by dropping rows or fields the tests never read; nothing is edited or made up.

| Folder | Source | Trim |
|---|---|---|
| `coingecko/` | `api.coingecko.com/api/v3` | `markets.json`: the first 20 of the 100 rows of `/coins/markets`. `coins_list.json`: the `/coins/list?include_platform=true` entries of those 20 coins plus `wrapped-monad` and `cuberium` (an empty Ethereum address) |
| `llama/` | `api.llama.fi`, `stablecoins.llama.fi` | `tvl.{monad,ethereum}.json`: the last 400 points of `/v2/historicalChainTvl/{Name}`. `stablecharts.*.json`: the last 400 days of `/stablecoincharts/{Name}`, reduced to `date` and `totalCirculatingUSD.peggedUSD`. `dex.*.json`: `/overview/dexs/{Name}` without `allChains`, the last 400 chart days and the 20 protocols with the most volume, without their `methodology`. `chains.json`: the two rows of `/v2/chains`. `protocols.json`: the 12 largest Monad and Ethereum rows of `/protocols` and two CEX/Chain rows of each, without description, dimensions, methodology and the other long text fields. `stablecoins.json`: the largest stablecoins on Monad and Ethereum from `/stablecoins?includePrices=true`, with `chainCirculating` reduced to those two chains |
| `blockscout/stats.*.json` | `eth.blockscout.com/stats-service/api/v1/lines/{id}`, the six ids of the activity route | 121 days, 2026-06-15 to 2026-10-13 |

