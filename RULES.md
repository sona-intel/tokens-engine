# How the token engine makes its numbers

Every number comes from public data. This file says where each one comes from, how it is computed, and where it falls short. The `coverage` codes in the answers point back here.

## Is it a token?

The engine calls `name()`, `symbol()`, `decimals()` and `totalSupply()` in one Multicall3 `aggregate3` call with every call allowed to fail. The address is a token when `decimals()` answers a value from 0 to 255 and `totalSupply()` answers a 32-byte word. An account without code answers every call with success and no data, so it fails both checks and is `not_found`. `name` and `symbol` are optional: an ABI string, or a `bytes32` for old tokens such as MKR, with NUL padding, control characters and invalid UTF-8 removed and at most 128 bytes kept.

## Amounts

`raw` is the contract's integer. `value` is `raw / 10^decimals`, written exactly with no trailing zeros. A `share` is `balance / total_supply`, rounded to 10 significant digits.

## Price, FDV and market cap

- `price_usd` is `current.usd` of Sona's prices service, which reads DefiLlama's free coins API. No price (`token_price_unknown`) leaves `price_usd` and `fdv_usd` null.
- `fdv_usd` is `total_supply.value * price_usd`, rounded to 12 significant digits.
- `market_cap_usd` is what a provider reports as circulating market cap: Blockscout's `circulating_market_cap` on Ethereum, GeckoTerminal's `market_cap_usd` on Monad and Arc. Both come from CoinGecko, which counts a token's supply on every chain. When the figure is more than 1% above this chain's `fdv_usd`, it cannot be this chain's own, and the record says `token_market_cap_multichain`. The engine reports it as given and never computes a market cap itself.

## Holder count and top 10 share

- Ethereum: Blockscout's `holders_count`, and the 10 largest balances of Blockscout's first holder page over the total supply.
- Monad and Arc: GeckoTerminal's `holders.count` and `distribution_percentage.top_10 / 100` (`token_holder_count_provider`), from the chain's own network (`gecko_network`, `monad` and `arc`). GeckoTerminal updates them on its own schedule (`last_updated` in its answer) and sends no distribution for some tokens, WMON among them. When it has no count and the index of the token is complete (`full` mode, `ready`), the count is the index's.

## Holder lists

### Ethereum

Blockscout's holder pages, 50 a page, ranked by Blockscout (`token_holders_ranked_by_provider`). The engine re-pages them to the asked `limit` and keeps each Blockscout page 10 minutes, so two pages read minutes apart can overlap or skip a holder whose balance moved in between.

### Monad and Arc: the index

Each of the two chains has its own index, with the same rules and settings unless a chain sets its own (`prewarm`, `activity_window_blocks`). The index reads the token's `Transfer(address,address,uint256)` logs from HyperSync, and for wrapped MON also `Deposit(address,uint256)` (a mint to the address) and `Withdrawal(address,uint256)` (a burn from it). It stores the current balance of each holder and deletes a holder whose balance reaches zero. The zero address is never a holder.

A backfill starts at block 0 in **`full` mode**: each page of logs adds `+amount` to the receiver and `-amount` to the sender, committed with the page's end block in one transaction, so a restart resumes where it stopped. Balances in this mode are exact for any token that only changes balances through these events (`token_balances_from_transfers`); a rebasing or fee-taking token drifts from `balanceOf`.

One HyperSync request covers what the server can scan in one go: about 4,000 to 5,000 Monad blocks wherever the token is active. After every page the engine projects the requests the rest of the backfill needs, `spent + (head - next_block) / blocks of the latest page`. When two pages in a row project past `backfill_budget` (120), the token switches to **`activity` mode**: its rows are cleared, the index starts again `activity_window_blocks` (20,000 Monad blocks, 14,400 Arc blocks, about two hours on each) before the head, and from then on every address a page touches gets its balance read with `balanceOf` at the latest block, in Multicall3 batches of 300. The list then holds every address that moved the token since `start_block`, with its exact current balance, and misses holders that have not moved it since (`token_holders_from_activity`). On 2026-10-13 USDC projected 368 and 892 requests in two runs, each after 3 or 4 pages, and WMON 266, so both run in `activity` mode. The projection reads the latest page only; sampled ranges of USDC's history cover 4,000 to 5,600 blocks a request from block 37M on, so a full USDC backfill would take about 17,000 requests, more than 18 hours of the whole key.

After the backfill the **tail** keeps every ready token current: once a minute one HyperSync query per chain for all of its tokens, from the lowest `next_block` to 10 blocks behind the archive height, each token applying only the logs at or after its own `next_block`.

At most one backfill runs at a time on each chain, and the index of each chain holds at most `max_tokens` (50) tokens. The indexes of both chains share the background quota of 3 HyperSync requests a minute. A token past that is `unavailable` (`token_index_unavailable`). A backfill that fails is retried after 10 minutes and is `unavailable` until then. A token in `queued` or `indexing` answers what is indexed so far (`token_index_partial`).

### Arc's USDC

USDC is Arc's native coin. `0x3600000000000000000000000000000000000000` is its ERC-20 view: the same balance, kept by the chain at 18 decimals and answered by `balanceOf` at 6 with the last 12 digits cut. The system address `0xfffffffffffffffffffffffffffffffffffffffe`, which has no code, logs a `Transfer` at 18 decimals for every move of the coin, plain sends included. A move made through the token contract (an ERC-20 transfer, an EIP-3009 settlement, a mint or burn) also gets a second `Transfer` from `0x3600…`, at 6 decimals. The index takes one emitter (`native` in the chain's config, `token_native_coin`):

- **Moves.** USDC's backfill and tail ask HyperSync for the system address's `Transfer` logs and never for the token contract's, and every amount is cut to 6 decimals (`raw / 10^12`, rounded down), so a move made through the token counts once and a plain send counts too. In the recorded Arc blocks 25,941,000 to 25,941,019 the system address logged 107 moves and the token contract 82, each of the 82 the same transaction, sender, receiver and amount as one of the 107; 17 transactions were plain sends, and 16 of the 107 amounts had digits below 6 decimals.
- **Balances.** Gas fees move the coin without a log (about 0.0004 USDC a transaction), so a balance summed from logs, or read when the holder last moved, over-states a holder that paid gas since. Every USDC balance the holder list and the holder map show is therefore the holder's native balance read with Multicall3's `getEthBalance` (the `BALANCE` the node reads for `eth_getBalance`; the recorded answers are equal), one `aggregate3` call a page, kept 2 minutes (`meta_cache`) and cut to 6 decimals. `activity` mode stores the same native balance for the addresses it touches. The order of the list and its cursor follow the stored balances, which can differ from the shown ones by the gas paid since. A page whose read fails answers 503.
- **Counts.** In `full` mode a holder that sent everything but its gas keeps a stored balance of that gas, so the index's own holder count can include emptied wallets. The cut of each move also adds up: a stored balance can be off by up to a millionth of a USDC for every move with digits below 6 decimals, and can drop below zero for an address that pays no gas, such as a contract. A busy USDC runs in `activity` mode, where the count is GeckoTerminal's.
- **Other tokens.** Every other Arc token (EURC, USYC, cirBTC, bridged WETH and the rest) reads its own contract's `Transfer` logs and `balanceOf`, as on Monad. Arc has no wrapped native coin.

## Large transfers

- Ethereum: the HyperSync logs of the token over the last `transfer_window_blocks` (7,200, about a day). The first page shows how many blocks one request covers; when the window does not fit in `transfer_page_budget` (3) requests, the window shrinks to what the requests left can cover, at least `transfer_min_blocks` (20), and that length is remembered for 6 hours (`token_transfer_window_cut`). The window always ends at the archive height when the read finished, and `window` gives its exact blocks and times. The 100 largest transfers are kept 15 minutes. A window is only data when HyperSync answered every log with its block time: an answer without them is a failure, and a failure is never cached. When HyperSync fails or its quota is spent, the answer is the last good window of that token, kept up to 6 hours beyond the 15 minutes, with `token_transfer_window_older` (`window.to_time` is the time it ends); only a token with no kept window answers 503. A cut window with no transfer in it is trusted for 1 minute only, not 15.
- Monad and Arc: the index keeps the 100 largest `Transfer` events of each UTC day for the last 7 days (`window_days`), pruned every hour. For Arc's USDC they are the system address's moves, plain sends included, cut to 6 decimals. The window starts at the first block of the oldest kept day (found by a search over block times) or at `start_block` when the index starts later, and ends at `indexed_block`.

Transfers are ranked by amount, then newest first. Mints come from and burns go to the zero address. Deposits and withdrawals of wrapped MON are not transfers.

## Holder map

`GET .../tokens/{token}/holder-map?limit=` (50 to 250, default 100) gives the `limit` largest holders as nodes and, on Ethereum, the transfers between them as links. Each `limit` is its own map.

### Nodes

The same list as the holder route, read the same way: on Ethereum Blockscout's holder pages (kept 10 minutes, `token_holders_ranked_by_provider`), so 250 nodes take 5 pages; on Monad and Arc the index, with native balances for Arc's USDC. A map there shows nodes only when the token's index is complete, `full` mode and `ready` (`token_balances_from_transfers`): an index in `activity` mode misses every holder that has not moved the token lately, and one still being built misses the rest of the history, and a map of such a list would put the wrong holders at the top. Those tokens answer no nodes (`token_holder_map_index_incomplete`, or `token_index_unavailable` when the index cannot take the token). The first call queues the token in the index like the holder route.

A node's `balance` is the holder's balance in token units, `share` as in the holder list, `label` Blockscout's name tag or contract name, and `kind` `contract` or `wallet` from Blockscout's `is_contract` (`unknown` on Monad and Arc).

### Links

Ethereum only; Monad and Arc answer `unavailable` (`token_holder_map_links_ethereum_only`). The index keeps no transfers but the largest of each day, and one HyperSync request covered about half an hour of Monad blocks for an active token in the index's measurements, so 30 days would take hundreds of requests, far past a map's budget.

A link is every `Transfer(address,address,uint256)` log of the token in the window whose sender (topic 1) and receiver (topic 2) are both nodes, added up per unordered pair: `transfers` counts both directions and `amount` sums them in token units. Self transfers and the zero address are left out. `from` is the lower of the two addresses, not the sender. The logs come from one HyperSync query of the token's address, topic 0 the `Transfer` signature, and topics 1 and 2 each the list of nodes as 32-byte topics.

- **Window.** The last 30 days (`window_days`): from the first block at or after the archive height's time minus 30 days (found by a search over block times through the RPC router) to the archive height.
- **Requests.** At most 8 HyperSync requests a map (`page_budget`), sent on the visitors' share of the quota (`hypersync_interactive_per_minute`, 2 a minute) at a lower priority than the transfer windows: a request of a map is sent only when no visitor request waits, when the bucket holds a second slot that stays free for a visitor arriving right after, and at least a minute after the previous map request, so a page load never waits behind a map and the total stays within the 2 a minute. A map takes 1 request a minute at most (8 requests, about 8 minutes), so the key's 15 a minute that x402-indexer mostly spends are never exceeded. A request that HyperSync refuses for the quota waits for the next quota window; after the client's own 3 tries the read asks twice more, then fails.
- **Busy tokens.** One request covers what HyperSync scans in one go. When the first request shows that the 7 left cannot reach the archive height at its pace, the read starts again at the archive height minus 90% of what 7 requests would cover, so the window ends at the newest block; if the requests still run out, the map keeps what was read. Either way `window_days` is the span read, to one decimal, from the times of its first and last block, and the map says `token_holder_map_window_cut`. On 2026-10-13 UNI's 30 days with its 100 largest holders took 3 requests (70,000 to 78,000 blocks each).
- **In the background.** A call never waits for HyperSync. It answers the stored links when they were read less than 12 hours ago (`ready`, `built_at`). Otherwise it answers `building` with no items and queues one read for the token and `limit`, unless one is queued or running. One read runs at a time, at most 20 wait, and a map past that answers `unavailable` (`token_holder_map_queue_full`). A read that fails answers `unavailable` (`token_holder_map_links_failed`) and is queued again by the first call 10 minutes later.
- **Stored.** Postgres table `holder_map`, one row per chain, token and `limit`: when it was read, its first and last block, and the pairs as JSON. Rows older than 7 days are deleted every hour. A pair whose holder has left the node list since the read is dropped from the answer.

A node's `cluster` comes from a union-find over the links of the answer: nodes that links connect, directly or through other nodes, are one cluster. `clusters` lists those of two or more nodes with their `size` and `share` (the sum of the members' balances over the total supply), largest share first; `id` is that rank from 1. A cluster says only that its holders moved the token between each other: one owner's wallets, an exchange and its hot wallets, or a pool and a router all look alike.

### Coverage codes of this record

| Code | Meaning |
|---|---|
| `token_holders_ranked_by_provider` | the Ethereum nodes are Blockscout's list |
| `token_balances_from_transfers` | the nodes on Monad and Arc are the full index's balances |
| `token_native_coin` | the token is Arc's USDC, so the nodes' balances are native balances |
| `token_holder_map_index_incomplete` | the index of the token is still being built or follows recent activity, so `nodes` is empty |
| `token_index_unavailable` | the index cannot take the token, so `nodes` is empty |
| `token_holder_map_links_ethereum_only` | links are read on Ethereum only |
| `token_holder_map_window_cut` | the links cover fewer than 30 days, `links.window_days` says how many |
| `token_holder_map_queue_full` | 20 maps wait for links, so this one was not queued |
| `token_holder_map_links_failed` | the last read failed, a call queues it again 10 minutes after the failure |

## Logos

`logo_url` of a token is the first https URL among: the `logoURI` of the curated lists (in list order: Uniswap default, then the Monad list), Blockscout's `icon_url` (Ethereum), GeckoTerminal's `image_url` (Monad and Arc). It is null when none gives one. A URL that is not https, has credentials, holds a space or control character, or is longer than 512 bytes is dropped, so GeckoTerminal's placeholder `missing_small.png` never shows.

## Token search

The curated lists are read from their public URLs when the engine starts and again every 24 hours, with a 20 s timeout each, and a list that fails keeps its last copy (a list that never loaded is tried again after 5 minutes, and the search says `token_search_list_unavailable`). The engine keeps the Ethereum (1), Monad (143) and Arc (5042) entries, drops the zero address (the native coin) and entries without a symbol, address or decimals. They are:

- Uniswap Labs' default list, `https://tokens.uniswap.org` (several chains; maintained by Uniswap Labs; 1,784 tokens on 2026-10-13, 411 on Ethereum and 6 on Monad). It comes first.
- The Monad Foundation's mainnet list, `https://raw.githubusercontent.com/monad-crypto/token-list/refs/heads/main/tokenlist-mainnet.json` (repository `monad-crypto/token-list`, 119 entries at version 2.49.0, new tokens by pull request, last commit 2026-10-02). Its own README says that being listed is not an endorsement.

CoinGecko's `tokens.coingecko.com/ethereum/all.json` was checked and left out: it holds 6,052 tokens and several copies of the same name (five different `PEPE`), so listing it would make copycats verified. Neither kept list has an Arc token, and CoinGecko's `tokens.coingecko.com/arc/all.json` (87 tokens on 2026-10-14) was left out for the same reason: beside the real USDC it lists two memecoins with the symbol `USDC` (`UpSideDownCat` at `0x8e98…f9d9` and `Fat Cat Bat Rat Wif Hat` at `0xb67f…1ed5`), two `DUKE` and two `ARCLIGHT`. So every Arc token is unverified and ranks by liquidity.

A token matches when its symbol starts with the query, else its name starts with it, else its symbol holds it, else its name holds it. A full address matches that token. Results are ranked by, in this order: on a list (`verified`) before not, the better match, an exact symbol, the earlier list, the deeper liquidity, the shorter symbol, then the address.

Tokens that are on no list come from two places. GeckoTerminal's pool search (`/search/pools?query=&network=&include=base_token,quote_token`, one request per chain, kept 5 minutes) gives the base and quote token of each pool found; a token's liquidity is the sum of `reserve_in_usd` of the pools found that hold it, which can be a copycat's own doing, so it ranks only among unverified tokens. The indexes of Monad and Arc add the tokens they hold, read from their contracts. The search waits at most 3 seconds for GeckoTerminal, behind token pages in the pacer, and without it answers with the curated tokens and `token_search_long_tail_unavailable`.

## Markets and chain figures

These routes answer from memory. A loop per source reads it in the background, replaces the held copy only when a read succeeds (a failed read leaves the last good copy and is retried after a tenth of the interval, between 1 and 10 minutes, or after the source's `Retry-After` when that is longer), and a call never waits for a source. Before the first read of a source succeeds its routes answer 503 `upstream_unavailable`. A copy that missed 3 intervals is **stale** and the answer says so in `coverage`. On 2026-10-13 all sources together held about 7 MB of heap and the whole process peaked at 32 MB resident (measured with `GOMEMLIMIT=160MiB`, the container's setting), so the 192 MB container limit stays.

### Top coins

`GET /internal/v1/markets/top-coins` serves CoinGecko's `/coins/markets?vs_currency=usd&order=market_cap_desc&per_page=100&page=1&price_change_percentage=24h`, read every 5 minutes.

- `rank` is `market_cap_rank`, `price_usd` is `current_price`, `market_cap_usd` is `market_cap`, `change_24h_pct` is `price_change_percentage_24h` rounded to 2 decimals (null when CoinGecko has none). A coin without a price or a market cap is left out. `symbol` is upper case, `logo_url` is CoinGecko's `image` when it is an https URL, else null.
- `as_of` is the newest `last_updated` of the list, or the time of the read when none parses. Prices are the ones CoinGecko had at that time, up to 5 minutes older than the call, more when stale.
- `tokens` come from `/coins/list?include_platform=true`, read once a day (3,976,033 bytes and 22,229 coins on 2026-10-13, decoded one coin at a time, only the platform keys `ethereum`, `monad` and `arc` kept). The Monad key is `monad` (195 coins held it, Ethereum's `ethereum` 6,057), Arc's is `arc` (USDC lists `0x3600…`). An address is kept when it is `0x` and 40 hex digits (CoinGecko sends an empty string for some coins), lowercased, Ethereum first, then Monad, then Arc. A coin with no contract on either chain (Bitcoin, Ethereum's own coin) has an empty list. The map is CoinGecko's: a wrapped or bridged token is whatever CoinGecko says it is, and two coins can list the same address.
- CoinGecko's keyless tier is shared and strict: the engine sends at most one CoinGecko request every 20 seconds (both reads share the pace), pauses for the `Retry-After` of a 429 (at least 1 minute), and when `COINGECKO_API_KEY` is set sends it as `x-cg-demo-api-key`. The key is never logged.

### DeFi figures

All come from DefiLlama's keyless APIs (`api.llama.fi`, `stablecoins.llama.fi`), at most 30 requests a minute, every hour. DefiLlama calls Ethereum `Ethereum`, Monad `Monad` and Arc `Arc` (names are case-sensitive in its paths); other chains answer 404 `unsupported_chain`. A day is a UTC day, and **the last item of every series is the current day, still in progress** (`chain_series_last_day_partial`).

- **TVL** (`/defi/tvl`): `/v2/historicalChainTvl/{Name}`, which answers for Monad and Arc. `current_usd` is its last point. The point is the same figure as `/v2/chains`: on 2026-10-13 Monad's last point was 1,031,397,405 against 1,031,397,356 there, and Ethereum's 51,215,175,507 against 51,307,498,418 (read minutes apart; the two endpoints refresh on their own schedules, so they differ by the movement in between). `change_1d_pct` and `change_7d_pct` compare the last point with the point exactly 1 and 7 days before its day, null when that day is missing; because the last point is the day so far, they measure the move since that earlier day's snapshot. Monad's first points (October to November 2025) are weeks apart, so `days=all` has gaps there. `days` keeps the points of the last N days.
- **Protocols** (`/defi/protocols`): `/protocols` (9,249,298 bytes and 8,714 protocols on 2026-10-13), decoded one protocol at a time, keeping the rows whose `chainTvls` holds the chain's name (147 for Monad, 1,762 for Ethereum). `tvl_usd` is `chainTvls[Name]`: the protocol's own TVL on this chain, without the separate `-borrowed`, `-staking`, `-pool2` and `-vesting` keys. The categories `CEX` (exchange custody) and `Chain` (a chain's own entry) are left out, because DefiLlama leaves them out of a chain's TVL, as are rows with no positive TVL. Even then the rows **do not add up to the chain's TVL** (`chain_protocols_not_additive`): on 2026-10-13 Ethereum's rows summed to 158.8 billion dollars against the chain's 51.3 billion (278.4 billion with CEX and Chain), and Monad's to 1.90 billion against 1.03 billion, because DefiLlama's chain figure also removes TVL that two protocols count at once (liquid staking, risk curators, allocators), and the protocol list does not say which. `change_1d_pct` and `change_7d_pct` are `change_1d` and `change_7d` of the protocol on **all its chains** (`chain_protocol_change_all_chains`), because the list holds no per-chain change. `categories` totals every kept row of the chain; `items` holds the 100 largest at most, and `limit` cuts that.
- **Stablecoins** (`/defi/stablecoins`): `items` are the 15 largest USD-pegged stablecoins from `/stablecoins?includePrices=true`, `chainCirculating[Name].current.peggedUSD`, positive only; stablecoins pegged to another currency are not listed (they have no `peggedUSD` figure). `history` is `/stablecoincharts/{Name}`, `totalCirculatingUSD.peggedUSD` of each day, the last 365. `total_usd` is the last history point, DefiLlama's chart total, which counts more than the listed coins: Monad's was 814,646,451 against 773,180,809 for the sum of its USD-pegged coins on 2026-10-13, and the gap was not explained by DefiLlama.
- **DEX volume** (`/defi/dexs`): `/overview/dexs/{Name}?excludeTotalDataChart=false&excludeTotalDataChartBreakdown=true`. `total_24h_usd`, `total_7d_usd`, `total_30d_usd` and `change_1d_pct` are DefiLlama's headline fields as sent. They are not sums of `items`: on 2026-10-13 Monad's `total24h` (33,337,989) was the chart's second-last day, while the sum of the protocol rows' `total24h` (36,632,955) matched the chart's last day, the day in progress. `items` are the chart's daily volumes. `protocols` are the 15 DEXes with the most `total24h` above zero, named by `displayName`, with `total24h` and `total7d`; a DEX that trades on many chains shows its volume on this chain.

### Activity

`GET /internal/v1/chains/eip155:1/activity` reads Blockscout's stats service (`eth.blockscout.com/stats-service/api/v1/lines/{id}`, `from` 364 days before today to today), six requests an hour through the same Blockscout pacer as the token routes (3 a second, at the background priority). Monad and Arc answer 404 `unsupported_chain` because no source is wired for them.

| Metric | Line id | Unit as sent | Output |
|---|---|---|---|
| `txs` | `newTxns` | transactions a day | as sent |
| `active_accounts` | `activeAccounts` | accounts that sent or received that day | as sent |
| `new_accounts` | `newAccounts` | accounts seen for the first time | as sent |
| `gas_price_gwei` | `averageGasPrice` | gwei (the line's `units`) | as sent |
| `fees_eth` | `txnsFee` | ETH (the line's `units`) | as sent |
| `utilization_pct` | `networkUtilization` | a fraction, 0.5 for a half full block | times 100, so 50.4 |

The utilization line has no unit in the service's description; it was read as a fraction because its days sit at 0.504 and Blockscout's own `/api/v2/stats` and blocks give 52.8% and 54% to 81% on the same day. `days` keeps the last N days.

### Coverage codes of these records

| Code | Meaning |
|---|---|
| `market_top_coins_stale` | the CoinGecko list is over 15 minutes old because its refresh keeps failing |
| `market_token_map_unavailable` | the address map has not loaded, or is over 3 days old, so `tokens` can be empty or out of date |
| `chain_defi_stale` | the DefiLlama copy is over 3 hours old |
| `chain_activity_stale` | the Blockscout stats copy is over 3 hours old |
| `chain_series_last_day_partial` | the last item is the current UTC day |
| `chain_protocols_not_additive` | protocol rows do not sum to the chain's TVL |
| `chain_protocol_change_all_chains` | protocol changes are those on all chains |

## Limits

- No number is a forecast, and nothing here labels addresses beyond Blockscout's own name tags on Ethereum.
- Each index trails the head by 10 blocks (4 seconds on Monad, 5 on Arc) plus up to a minute of tail interval, more while the HyperSync quota is spent.
- Cached answers can be up to their cache time old: contract reads and Arc's native balances 2 minutes, prices 1 minute, Blockscout 10 minutes, GeckoTerminal and Ethereum transfer windows 15 minutes, a GeckoTerminal search 5 minutes, a token list up to a day, holder map links 12 hours.
- A token on a curated list is not safe or endorsed, and a token on no list is not a scam: `verified` only says which of the two the lists can tell. A token that was added to a list after the last read shows as unverified until the next one.
