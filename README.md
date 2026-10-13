# Sona Intel token engine

Sona's token data for token pages on Ethereum (`eip155:1`) and Monad (`eip155:143`). For any ERC-20 token it answers:

- what it is: name, symbol, decimals and total supply, read from the contract;
- what it is worth: price, fully diluted value and the market cap a provider reports;
- who holds it: holder count, the share of the top 10 holders, and the holder list, largest first;
- what moved: the largest transfers of a recent window.

[`RULES.md`](RULES.md) says how every number is made and where it falls short.

## Read-only and keyless

The engine only reads. It holds no wallet or private key and signs nothing. Its only credential is the HyperSync key, read from `ENVIO_HYPERSYNC_KEY` and never logged or echoed. Never commit real keys; copy `.env.example` to `.env`.

## Data sources

| Source | Used for |
|---|---|
| JSON-RPC (in the Sona stack the RPC router, `http://rpc-router:4000/sona/{ethereum,monad}`) | `name`, `symbol`, `decimals` and `totalSupply` in one Multicall3 `aggregate3` call, `balanceOf` batches for the Monad index, block times. Every request carries `X-ERPC-Use-Upstream: !drpc-*`, so the router never uses its paid dRPC upstreams, and the engine counts any call the router still billed |
| Sona's prices service (`http://prices:8080`) | the current price, from its 1-day chart |
| Blockscout (`eth.blockscout.com`, keyless, at most 3 requests a second) | Ethereum: holder count, circulating market cap, holder pages |
| GeckoTerminal (`api.geckoterminal.com`, keyless, at most 20 requests a minute) | Monad: holder count, top 10 share, market cap |
| Envio HyperSync (`ENVIO_HYPERSYNC_KEY`) | Ethereum: the transfer window of a token. Monad: the backfill and the tail of the holder index |
| Postgres | the Monad index: indexed tokens, current balances, the largest transfers of each day of the last 7 |

### The HyperSync quota

The key allows 15 requests a minute, shared with Sona's x402 indexer, which spends most of each minute within seconds of its start. The engine keeps to 3 requests a minute for the index and 2 for visitors (`hypersync_per_minute`, `hypersync_interactive_per_minute`). It reads the quota headers of every answer: when a window is spent it waits until a second after the reset and then sends its share at once. A 429 waits the same way.

One HyperSync request covers about 4,000 to 5,000 Monad blocks (half an hour of chain) wherever a token is active, and only a few dozen Ethereum blocks for a busy token. That is why the Monad index has two modes and the Ethereum window shrinks for busy tokens; `RULES.md` has the details.

## API

Internal only. `sona-api` is the public face. The full contract is [`openapi.yaml`](openapi.yaml) (OpenAPI 3.1, shown in the stack's Swagger as "Token service (internal)"). Answers are bare records with a `kind`, the same shape as the other Sona engines, and errors are `application/problem+json` with the shared codes (`invalid_query`, `invalid_id`, `unsupported_chain`, `not_found`, `upstream_unavailable` with `retry_after_s`, `deadline_exceeded`, `internal`).

| Route | Answers |
|---|---|
| `GET /internal/v1/chains/{chain}/tokens/{token}` | `token_info`: name, symbol, decimals, total supply, price, FDV, market cap, holder count, top 10 share, sources, coverage |
| `GET .../tokens/{token}/holders?limit=&cursor=` | `token_holders`: holders largest first with balance and share, `next_cursor`, holder count, source, and the Monad `index` state |
| `GET .../tokens/{token}/transfers?limit=` | `token_transfers`: the largest transfers of the window, with the window's blocks and times |
| `GET /internal/v1/status` | chains, paid RPC calls, HyperSync use, and every indexed Monad token with its state, mode, blocks and backfill cost |
| `GET /healthz`, `GET /readyz` | liveness, and readiness (the database answers) |

An address that answers neither `decimals()` nor `totalSupply()` is no token and answers 404 `not_found`. A Monad token whose index is `queued` or `indexing` answers 200 with partial data and the `index` object, never an error. Metrics are on port 9464 (`/metrics`, not published; Prometheus scrapes it as the job `tokens-engine`).

## Configuration

`config/engine.yaml` holds the chains (CAIP-2 id, RPC and HyperSync URLs, where holders come from, the wrapped native coin), the source URLs and rates, the cache times, the Ethereum transfer window and the index settings (`max_tokens` 50, `backfill_budget` 120 requests, `activity_window_blocks` 20,000, `tail_interval` 60 s, `lag_blocks` 10, `window_days` 7, `top_per_day` 100, `prewarm` USDC and WMON). It is validated on start, and a key it does not know is an error. Settings from the environment (names in `.env.example`):

| Variable | Use |
|---|---|
| `TOKENS_CONFIG_DIR` | folder holding `engine.yaml` (default `/etc/tokens`) |
| `TOKENS_RPC_ETHEREUM`, `TOKENS_RPC_MONAD` | JSON-RPC endpoints, override the file |
| `TOKENS_PRICES_URL` | the prices service, overrides the file |
| `ENVIO_HYPERSYNC_KEY` | HyperSync, required for the Monad index |
| `PGHOST`, `PGPORT`, `PGUSER`, `PGDATABASE`, `PGPASSWORD` | Postgres, read by the libpq variables |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error` |

## Run

In the Sona stack, from `services/` (needs `TOKENS_PG_PASSWORD`, `TOKENS_PG_ENGINE_PASSWORD` and `ENVIO_HYPERSYNC_KEY` in `services/.env`; the RPC router and the prices service should be up):

```sh
docker compose up -d --build --no-deps tokens-postgres tokens-engine
curl -s http://127.0.0.1:8221/internal/v1/chains/eip155:143/tokens/0x754704bc059f8c67012fed69bc8a327a5aafb603
curl -s http://127.0.0.1:8221/internal/v1/status
```

The engine listens on host port 8221 and Postgres on 5436, both bound to `127.0.0.1`. The compose file is `services/platform/tokens/compose.yaml`, with `mem_limit` 192 MB for the engine (`GOMEMLIMIT` 160 MiB) and 384 MB for Postgres. The engine connects as the least-privilege role `tokens_engine` (no superuser, createdb or createrole, default `search_path` `tokens`), which `initdb/01-engine-role.sh` creates on a fresh volume; `TOKENS_PG_PASSWORD` is only the password of the superuser `tokens`. A changed `config/engine.yaml` needs `docker compose up -d --force-recreate --no-deps tokens-engine`.

Outside Docker, with a Postgres reachable through the libpq variables:

```sh
TOKENS_CONFIG_DIR=config TOKENS_RPC_ETHEREUM=http://127.0.0.1:4000/sona/ethereum TOKENS_RPC_MONAD=http://127.0.0.1:4000/sona/monad \
  TOKENS_PRICES_URL=http://127.0.0.1:8210 go run . serve
```

The binary takes one subcommand: `serve` (the default), `healthcheck` (GET `http://127.0.0.1:8080/healthz`, exit 0 on 200) and `migrate`.

## Tests and lint

```sh
go build ./... && go vet ./...
go test ./...
golangci-lint run ./...
```

Tests need no network. The ones that need Postgres skip unless `TOKENS_TEST_PG` names a database where they may create and drop their own schemas, for example `TOKENS_TEST_PG="host=127.0.0.1 port=5436 user=tokens dbname=tokens"` with `PGPASSWORD` set. `testdata/` holds real answers of the RPC router, Blockscout, GeckoTerminal, HyperSync and the prices service, and its README says how each was recorded. The server tests check every route's answers, errors included, against `openapi.yaml`.

## Layout

| Path | Content |
|---|---|
| `main.go` | subcommands `serve`, `healthcheck`, `migrate` |
| `internal/config` | `engine.yaml` and environment, with the key redacted |
| `internal/evm` | addresses, ERC-20 selectors and topics, Multicall3 encode and decode |
| `internal/rpc` | JSON-RPC client with retries, the upstream filter header and a paid-call count |
| `internal/sources` | Blockscout, GeckoTerminal, prices and HyperSync clients, the pacer |
| `internal/cache` | in-memory answers with expiry, one fetch per key at a time |
| `internal/index` | the Monad index: decoding, backfill, mode switch and tail |
| `internal/store`, `migrations` | Postgres access and embedded SQL |
| `internal/tokens` | the token records |
| `internal/server` | HTTP handlers, parameters, problem mapping, metrics |
| `internal/app` | wiring for `main` |
| `internal/problem` | problem+json writer |
| `internal/testkit`, `internal/fixture` | test helpers on the recorded answers |

## License

Not chosen yet. The founder decides before the repository is made public.
