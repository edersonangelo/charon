-- A destination has no way to tell a delivery from Charon apart from anything
-- else that reaches its address. Signing is what closes that, and a set rather
-- than a column because rotating a secret means both are live for a while:
-- every one of these signs, and the receiver accepts whichever it already
-- knows.
--
-- The reference names where the secret is, never the secret. "env:NAME" reads
-- a variable; "file:/path" reads a file, which is what lets a secret rotate
-- without redeploying the process that signs.
create table signing_secret (
    id             uuid         primary key default uuidv7(),
    tenant_id      uuid         not null references tenant (id) on delete cascade,
    destination_id uuid         not null references destination (id) on delete cascade,
    reference      varchar(200) not null,
    created_at     timestamptz  not null default now(),
    unique (destination_id, reference),
    constraint signing_secret_reference_is_located
        check (reference ~ '^(env:[A-Za-z_][A-Za-z0-9_]*|file:/.+)$')
);

create index signing_secret_destination_idx on signing_secret (destination_id);

alter table signing_secret enable row level security;
alter table signing_secret force row level security;

create policy tenant_isolation on signing_secret
    using (tenant_id = current_tenant())
    with check (tenant_id = current_tenant());
