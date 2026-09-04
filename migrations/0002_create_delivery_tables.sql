create table destination (
    id         uuid        primary key,
    name       text        not null unique,
    url        text        not null,
    enabled    boolean     not null default true,
    created_at timestamptz not null default now()
);

create table route (
    id             uuid        primary key,
    provider       text        not null,
    destination_id uuid        not null references destination (id) on delete cascade,
    created_at     timestamptz not null default now(),
    unique (provider, destination_id)
);

create index route_provider_idx on route (provider);

alter table inbound_event add column planned_at timestamptz;

create index inbound_event_unplanned_idx
    on inbound_event (received_at)
    where planned_at is null;

-- A delivery is closed only by the destination answering 2xx. Reaching the
-- attempt limit closes it as dead, which is never counted as delivered.
create table delivery (
    id              uuid        primary key,
    event_id        uuid        not null references inbound_event (id) on delete cascade,
    destination_id  uuid        not null references destination (id) on delete cascade,
    state           text        not null,
    attempts        integer     not null default 0,
    next_attempt_at timestamptz not null,
    leased_until    timestamptz,
    last_status     integer,
    last_error      text,
    delivered_at    timestamptz,
    created_at      timestamptz not null default now(),
    unique (event_id, destination_id),
    constraint delivery_state_known check (state in ('pending', 'delivered', 'dead'))
);

create index delivery_claimable_idx
    on delivery (next_attempt_at)
    where state = 'pending';

create index delivery_event_idx on delivery (event_id);
