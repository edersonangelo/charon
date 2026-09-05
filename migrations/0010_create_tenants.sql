create table tenant (
    id         uuid        primary key,
    slug       text        not null unique,
    name       text        not null,
    created_at timestamptz not null default now(),
    constraint tenant_slug_shape check (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$')
);

-- One tenant always exists, so a deployment that never wants more than one
-- never has to know the concept is there.
insert into tenant (id, slug, name)
values ('00000000-0000-0000-0000-000000000001', 'default', 'Default');

-- Carried on every scoped table rather than reached through a join, so a row
-- level policy is a comparison and not a subquery.
alter table inbound_event    add column tenant_id uuid not null default '00000000-0000-0000-0000-000000000001' references tenant (id) on delete cascade;
alter table inbound_request  add column tenant_id uuid not null default '00000000-0000-0000-0000-000000000001' references tenant (id) on delete cascade;
alter table delivery         add column tenant_id uuid not null default '00000000-0000-0000-0000-000000000001' references tenant (id) on delete cascade;
alter table delivery_attempt add column tenant_id uuid not null default '00000000-0000-0000-0000-000000000001' references tenant (id) on delete cascade;
alter table destination      add column tenant_id uuid not null default '00000000-0000-0000-0000-000000000001' references tenant (id) on delete cascade;
alter table route            add column tenant_id uuid not null default '00000000-0000-0000-0000-000000000001' references tenant (id) on delete cascade;
alter table provider         add column tenant_id uuid not null default '00000000-0000-0000-0000-000000000001' references tenant (id) on delete cascade;

-- The identity tables are deliberately not scoped: they are what establishes
-- which tenant a request belongs to, so they cannot be governed by it.
alter table panel_user add column tenant_id uuid not null default '00000000-0000-0000-0000-000000000001' references tenant (id) on delete cascade;

-- A name only has to be unique inside its tenant.
alter table provider    drop constraint provider_pkey;
alter table provider    add  constraint provider_pkey primary key (tenant_id, name);
alter table destination drop constraint destination_name_key;
alter table destination add  constraint destination_name_key unique (tenant_id, name);
alter table route       drop constraint route_provider_destination_id_key;
alter table route       add  constraint route_provider_destination_id_key unique (tenant_id, provider, destination_id);

create index inbound_event_tenant_received_idx on inbound_event (tenant_id, received_at desc);
create index delivery_tenant_state_idx         on delivery (tenant_id, state);
create index route_tenant_provider_idx         on route (tenant_id, provider);
create index panel_user_tenant_idx             on panel_user (tenant_id);
