-- Chain activity that the engine counts from blocks, one row per chain (CAIP-2 id) and UTC day: the blocks, their gas used
-- and gas limit, the transactions other than the chain's system transactions and the sum of their gas prices in wei.
-- A page's figures are added in the same transaction that moves the cursor, so no block is counted twice.
create table if not exists chain_stats_day (
    chain_id      text not null,
    day           date not null,
    blocks        bigint not null check (blocks >= 0),
    gas_used      numeric(40, 0) not null check (gas_used >= 0),
    gas_limit     numeric(40, 0) not null check (gas_limit >= 0),
    txs           bigint not null check (txs >= 0),
    gas_price_wei numeric(60, 0) not null check (gas_price_wei >= 0),
    primary key (chain_id, day)
);

-- The next block to count on each chain. first_time is the time of the first block counted: that day began before the
-- count did, so it is never served.
create table if not exists chain_stats_cursor (
    chain_id   text primary key,
    next_block bigint not null check (next_block >= 0),
    first_time timestamptz not null,
    updated_at timestamptz not null
);
