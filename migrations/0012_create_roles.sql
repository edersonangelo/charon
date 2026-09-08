-- A role is a name for a set of permissions, and that is configuration: a
-- deployment defines its own. The permissions themselves are not stored as an
-- open set, because each one names something the code checks.
create table role (
    tenant_id   uuid        not null references tenant (id) on delete cascade,
    name        text        not null,
    description text        not null default '',
    built_in    boolean     not null default false,
    created_at  timestamptz not null default now(),
    primary key (tenant_id, name),
    constraint role_name_shape check (name ~ '^[a-z0-9][a-z0-9-]{0,62}$')
);

create table role_permission (
    tenant_id  uuid not null,
    role       text not null,
    permission text not null,
    primary key (tenant_id, role, permission),
    foreign key (tenant_id, role) references role (tenant_id, name) on delete cascade
);

-- Which tenant an identity lands in, and as what. The key excludes the tenant
-- on purpose: one claim cannot lead to two tenants.
create table role_mapping (
    method     text        not null,
    claim      text        not null,
    tenant_id  uuid        not null references tenant (id) on delete cascade,
    role       text        not null,
    created_at timestamptz not null default now(),
    primary key (method, claim),
    foreign key (tenant_id, role) references role (tenant_id, name) on delete cascade
);

create index role_mapping_tenant_idx on role_mapping (tenant_id);

-- An operator's role, so a local account and one from single sign-on are the
-- same kind of thing.
alter table panel_user add column role text;

-- Seeding a tenant with the shipped roles is a function, because it has to
-- happen for every tenant created from now on and not only the first.
create function seed_roles(a_tenant uuid) returns void
    language plpgsql
as $$
declare
    seeded record;
begin
    for seeded in
        select * from (values
            ('viewer',   'read what arrived and where it went, and nothing else',
             array['events.read','routes.read','verification.read','operators.read']),
            ('operator', 'everything a viewer can, and send an event again',
             array['events.read','routes.read','verification.read','operators.read',
                   'events.replay']),
            ('admin',    'everything an operator can, and configure this tenant',
             array['events.read','routes.read','verification.read','operators.read',
                   'events.replay','routes.write','verification.write','operators.write',
                   'roles.write']),
            ('owner',    'everything an admin can, and manage tenants',
             array['events.read','routes.read','verification.read','operators.read',
                   'events.replay','routes.write','verification.write','operators.write',
                   'roles.write','tenants.write'])
        ) as t(name, description, grants)
    loop
        insert into role (tenant_id, name, description, built_in)
        values (a_tenant, seeded.name, seeded.description, true)
        on conflict (tenant_id, name) do nothing;

        insert into role_permission (tenant_id, role, permission)
        select a_tenant, seeded.name, unnest(seeded.grants)
        on conflict do nothing;
    end loop;
end
$$;

select seed_roles(id) from tenant;

-- Whoever already exists administers what already exists.
update panel_user set role = 'owner' where role is null;

alter table panel_user
    alter column role set not null,
    add constraint panel_user_role_fkey
        foreign key (tenant_id, role) references role (tenant_id, name);

alter table role            enable row level security;
alter table role            force  row level security;
alter table role_permission enable row level security;
alter table role_permission force  row level security;

create policy tenant_isolation on role
    using (tenant_id = current_tenant()) with check (tenant_id = current_tenant());
create policy tenant_isolation on role_permission
    using (tenant_id = current_tenant()) with check (tenant_id = current_tenant());
