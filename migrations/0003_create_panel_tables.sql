create table panel_user (
    id            uuid        primary key,
    email         text        not null unique,
    password_hash text        not null,
    created_at    timestamptz not null default now()
);

create table panel_session (
    token      bytea       primary key,
    user_id    uuid        not null references panel_user (id) on delete cascade,
    created_at timestamptz not null default now(),
    expires_at timestamptz not null
);

create index panel_session_expires_at_idx on panel_session (expires_at);

-- One row per attempt, so that resetting a delivery for replay does not erase
-- what was already tried. The panel reads this to show a timeline.
create table delivery_attempt (
    id           uuid        primary key,
    delivery_id  uuid        not null references delivery (id) on delete cascade,
    attempt      integer     not null,
    attempted_at timestamptz not null default now(),
    status       integer,
    error        text,
    duration_ms  integer     not null
);

create index delivery_attempt_delivery_idx
    on delivery_attempt (delivery_id, attempted_at desc);

alter table delivery add column replay_count integer     not null default 0;
alter table delivery add column replayed_at  timestamptz;
