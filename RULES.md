# How the token engine makes its numbers

Every number comes from public data. This file says where each one comes from, how it is computed, and where it falls short. The `coverage` codes in the answers point back here.

## Is it a token?

The engine calls `name()`, `symbol()`, `decimals()` and `totalSupply()` in one Multicall3 `aggregate3` call with every call allowed to fail. The address is a token when `decimals()` answers a value from 0 to 255 and `totalSupply()` answers a 32-byte word. An account without code answers every call with success and no data, so it fails both checks and is `not_found`. `name` and `symbol` are optional: an ABI string, or a `bytes32` for old tokens such as MKR, with NUL padding, control characters and invalid UTF-8 removed and at most 128 bytes kept.

## Amounts

`raw` is the contract's integer. `value` is `raw / 10^decimals`, written exactly with no trailing zeros. A `share` is `balance / total_supply`, rounded to 10 significant digits.

## Price, FDV and market cap

- `price_usd` is `current.usd` of Sona's prices service, which reads DefiLlama's free coins API. No price (`token_price_unknown`) leaves `price_usd` and `fdv_usd` null.
- `fdv_usd` is `total_supply.value * price_usd`, rounded to 12 significant digits.
- `market_cap_usd` is what a provider reports as circulating market cap: Blockscout's `circulating_market_cap` on Ethereum, GeckoTerminal's `market_cap_usd` on Monad. Both come from CoinGecko, which counts a token's supply on every chain. When the figure is more than 1% above this chain's `fdv_usd`, it cannot be this chain's own, and the record says `token_market_cap_multichain`. The engine reports it as given and never computes a market cap itself.

## Holder count and top 10 share

- Ethereum: Blockscout's `holders_count`, and the 10 largest balances of Blockscout's first holder page over the total supply.
- Monad: GeckoTerminal's `holders.count` and `distribution_percentage.top_10 / 100` (`token_holder_count_provider`). GeckoTerminal updates them on its own schedule (`last_updated` in its answer) and sends no distribution for some tokens, WMON among them. When it has no count and the index of the token is complete (`full` mode, `ready`), the count is the index's.

## Holder lists

### Ethereum

Blockscout's holder pages, 50 a page, ranked by Blockscout (`token_holders_ranked_by_provider`). The engine re-pages them to the asked `limit` and keeps each Blockscout page 10 minutes, so two pages read minutes apart can overlap or skip a holder whose balance moved in between.

### Monad: the index

The index reads the token's `Transfer(address,address,uint256)` logs from HyperSync, and for wrapped MON also `Deposit(address,uint256)` (a mint to the address) and `Withdrawal(address,uint256)` (a burn from it). It stores the current balance of each holder and deletes a holder whose balance reaches zero. The zero address is never a holder.

A backfill starts at block 0 in **`full` mode**: each page of logs adds `+amount` to the receiver and `-amount` to the sender, committed with the page's end block in one transaction, so a restart resumes where it stopped. Balances in this mode are exact for any token that only changes balances through these events (`token_balances_from_transfers`); a rebasing or fee-taking token drifts from `balanceOf`.

One HyperSync request covers what the server can scan in one go: about 4,000 to 5,000 Monad blocks wherever the token is active. After every page the engine projects the requests the rest of the backfill needs, `spent + (head - next_block) / blocks of the latest page`. When two pages in a row project past `backfill_budget` (120), the token switches to **`activity` mode**: its rows are cleared, the index starts again `activity_window_blocks` (20,000, about two hours) before the head, and from then on every address a page touches gets its balance read with `balanceOf` at the latest block, in Multicall3 batches of 300. The list then holds every address that moved the token since `start_block`, with its exact current balance, and misses holders that have not moved it since (`token_holders_from_activity`). On 2026-10-13 USDC projected 368 and 892 requests in two runs, each after 3 or 4 pages, and WMON 266, so both run in `activity` mode. The projection reads the latest page only; sampled ranges of USDC's history cover 4,000 to 5,600 blocks a request from block 37M on, so a full USDC backfill would take about 17,000 requests, more than 18 hours of the whole key.

After the backfill the **tail** keeps every ready token current: once a minute one HyperSync query for all of them, from the lowest `next_block` to 10 blocks behind the archive height, each token applying only the logs at or after its own `next_block`.

At most one backfill runs at a time and the index holds at most `max_tokens` (50) tokens. A token past that is `unavailable` (`token_index_unavailable`). A backfill that fails is retried after 10 minutes and is `unavailable` until then. A token in `queued` or `indexing` answers what is indexed so far (`token_index_partial`).

## Large transfers

- Ethereum: the HyperSync logs of the token over the last `transfer_window_blocks` (7,200, about a day). The first page shows how many blocks one request covers; when the window does not fit in `transfer_page_budget` (3) requests, the window shrinks to what the requests left can cover, at least `transfer_min_blocks` (20), and that length is remembered for 6 hours (`token_transfer_window_cut`). The window always ends at the archive height when the read finished, and `window` gives its exact blocks and times. The 100 largest transfers are kept 15 minutes.
- Monad: the index keeps the 100 largest `Transfer` events of each UTC day for the last 7 days (`window_days`), pruned every hour. The window starts at the first block of the oldest kept day (found by a search over block times) or at `start_block` when the index starts later, and ends at `indexed_block`.

Transfers are ranked by amount, then newest first. Mints come from and burns go to the zero address. Deposits and withdrawals of wrapped MON are not transfers.

## Logos

`logo_url` of a token is the first https URL among: the `logoURI` of the curated lists (in list order: Uniswap default, then the Monad list), Blockscout's `icon_url` (Ethereum), GeckoTerminal's `image_url` (Monad). It is null when none gives one. A URL that is not https, has credentials, holds a space or control character, or is longer than 512 bytes is dropped, so GeckoTerminal's placeholder `missing_small.png` never shows.

## Token search

The curated lists are read from their public URLs when the engine starts and again every 24 hours, with a 20 s timeout each, and a list that fails keeps its last copy (a list that never loaded is tried again after 5 minutes, and the search says `token_search_list_unavailable`). The engine keeps the Ethereum (1) and Monad (143) entries, drops the zero address (the native coin) and entries without a symbol, address or decimals. They are:

- Uniswap Labs' default list, `https://tokens.uniswap.org` (several chains; maintained by Uniswap Labs; 1,784 tokens on 2026-10-13, 411 on Ethereum and 6 on Monad). It comes first.
- The Monad Foundation's mainnet list, `https://raw.githubusercontent.com/monad-crypto/token-list/refs/heads/main/tokenlist-mainnet.json` (repository `monad-crypto/token-list`, 119 entries at version 2.49.0, new tokens by pull request, last commit 2026-10-02). Its own README says that being listed is not an endorsement.

CoinGecko's `tokens.coingecko.com/ethereum/all.json` was checked and left out: it holds 6,052 tokens and several copies of the same name (five different `PEPE`), so listing it would make copycats verified.

A token matches when its symbol starts with the query, else its name starts with it, else its symbol holds it, else its name holds it. A full address matches that token. Results are ranked by, in this order: on a list (`verified`) before not, the better match, an exact symbol, the earlier list, the deeper liquidity, the shorter symbol, then the address.

Tokens that are on no list come from two places. GeckoTerminal's pool search (`/search/pools?query=&network=&include=base_token,quote_token`, one request per chain, kept 5 minutes) gives the base and quote token of each pool found; a token's liquidity is the sum of `reserve_in_usd` of the pools found that hold it, which can be a copycat's own doing, so it ranks only among unverified tokens. The Monad index adds the tokens it holds, read from their contracts. The search waits at most 3 seconds for GeckoTerminal, behind token pages in the pacer, and without it answers with the curated tokens and `token_search_long_tail_unavailable`.

## Limits

- No number is a forecast, and nothing here labels addresses beyond Blockscout's own name tags on Ethereum.
- The Monad index trails the head by 10 blocks plus up to a minute of tail interval, more while the HyperSync quota is spent.
- Cached answers can be up to their cache time old: contract reads 2 minutes, prices 1 minute, Blockscout 10 minutes, GeckoTerminal and Ethereum transfer windows 15 minutes, a GeckoTerminal search 5 minutes, a token list up to a day.
- A token on a curated list is not safe or endorsed, and a token on no list is not a scam: `verified` only says which of the two the lists can tell. A token that was added to a list after the last read shows as unverified until the next one.
