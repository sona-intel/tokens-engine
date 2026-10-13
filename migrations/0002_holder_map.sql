-- The links of a holder map: transfers between a token's largest holders over a recent window, one row per chain
-- (CAIP-2 id), token and number of holders. links holds the window's length in days and the linked pairs. The engine
-- builds a row again after 12 hours and deletes rows older than 7 days.
create table holder_map (
    chain_id          text not null,
    token             bytea not null check (octet_length(token) = 20),
    "limit"           integer not null check ("limit" between 1 and 1000),
    built_at          timestamptz not null,
    window_from_block bigint not null,
    window_to_block   bigint not null,
    links             jsonb not null,
    primary key (chain_id, token, "limit")
);
