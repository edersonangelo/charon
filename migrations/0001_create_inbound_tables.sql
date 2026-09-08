create table inbound_event (
    id          uuid        primary key,
    provider    text        not null,
    path        text        not null,
    received_at timestamptz not null,
    body_size   integer     not null
);

create index inbound_event_received_at_idx
    on inbound_event (received_at desc);

create index inbound_event_provider_received_at_idx
    on inbound_event (provider, received_at desc);

-- Separate from inbound_event so retention is a delete against this table
-- alone: the record of what arrived survives, and secrets carried in headers
-- do not outlive the body they authenticated.
create table inbound_request (
    event_id uuid  primary key references inbound_event (id) on delete cascade,
    headers  jsonb not null,
    body     bytea not null
);
