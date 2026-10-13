-- One row per token the engine indexes on Monad. next_block is exclusive: every block below it is applied.
create table indexed_tokens (
    id            integer generated always as identity primary key,
    chain         text not null,
    token         bytea not null check (octet_length(token) = 20),
    state         text not null check (state in ('queued', 'indexing', 'ready', 'failed')),
    mode          text not null default 'full' check (mode in ('full', 'activity')),
    decimals      smallint not null check (decimals between 0 and 255),
    start_block   bigint not null default 0,
    next_block    bigint not null default 0,
    holders       bigint not null default 0,
    requests      integer not null default 0,
    requested_at  timestamptz not null default now(),
    started_at    timestamptz,
    ready_at      timestamptz,
    backfill_s    double precision,
    last_error    text,
    unique (chain, token)
);

-- Current balances only, a holder whose balance drops to zero is deleted
create table balances (
    token_id integer not null references indexed_tokens (id) on delete cascade,
    holder   bytea not null check (octet_length(holder) = 20),
    balance  numeric not null,
    primary key (token_id, holder)
);

create index balances_rank on balances (token_id, balance desc, holder);

-- The largest transfers of each UTC day over the last days, a bounded number per day
create table large_transfers (
    token_id  integer not null references indexed_tokens (id) on delete cascade,
    day       date not null,
    block     bigint not null,
    log_index integer not null,
    tx        bytea not null check (octet_length(tx) = 32),
    ts        timestamptz not null,
    sender    bytea not null check (octet_length(sender) = 20),
    recipient bytea not null check (octet_length(recipient) = 20),
    amount    numeric not null,
    primary key (token_id, block, log_index)
);

create index large_transfers_rank on large_transfers (token_id, amount desc, block desc, log_index desc);
create index large_transfers_day on large_transfers (token_id, day);
