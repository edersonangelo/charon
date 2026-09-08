-- The permissions are ours: each one names something the code checks, so the
-- set is closed and the database holds it as rows rather than as free text
-- repeated on every grant. A grant that names a permission nothing enforces
-- now fails on a foreign key instead of on a code path someone might skip.
create table permission (
    id          uuid        primary key default uuidv7(),
    name        text        not null unique,
    description text        not null default '',
    constraint permission_name_shape check (name ~ '^[a-z]+\.[a-z]+$')
);

insert into permission (name, description) values
    ('events.read',        'search events and read what arrived and where it went'),
    ('events.replay',      'send a recorded event to its destinations again'),
    ('routes.read',        'see where each provider''s events are delivered'),
    ('routes.write',       'add, change and remove routes and destinations'),
    ('verification.read',  'see how each provider''s requests are checked'),
    ('verification.write', 'change how requests are checked, and recheck refused ones'),
    ('operators.read',     'see who can sign in'),
    ('operators.write',    'add and remove operators, and change their role'),
    ('roles.write',        'define roles and what they may do'),
    ('tenants.write',      'create and remove tenants');

-- A role gets an identifier of its own, so what grants it points at the role
-- and not at a name that a rename would leave behind.
alter table role add column id uuid not null default uuidv7();
alter table role drop constraint role_pkey cascade;
alter table role add constraint role_pkey primary key (id);
alter table role add constraint role_tenant_id_name_key unique (tenant_id, name);

alter table panel_user add constraint panel_user_role_fkey
    foreign key (tenant_id, role) references role (tenant_id, name)
    on update cascade;
alter table role_mapping add constraint role_mapping_tenant_id_role_fkey
    foreign key (tenant_id, role) references role (tenant_id, name)
    on update cascade on delete cascade;

-- The grant becomes what it always was: a tenant, a role and a permission.
create table role_grant (
    tenant_id     uuid not null references tenant (id) on update cascade on delete cascade,
    role_id       uuid not null references role (id) on delete cascade,
    permission_id uuid not null references permission (id) on delete cascade,
    primary key (role_id, permission_id)
);

create index role_grant_tenant_idx on role_grant (tenant_id);

insert into role_grant (tenant_id, role_id, permission_id)
select rp.tenant_id, r.id, p.id
from role_permission rp
join role r on r.tenant_id = rp.tenant_id and r.name = rp.role
join permission p on p.name = rp.permission;

drop table role_permission;

alter table role_grant enable row level security;
alter table role_grant force  row level security;
create policy tenant_isolation on role_grant
    using (tenant_id = current_tenant()) with check (tenant_id = current_tenant());

-- Seeding a tenant reads the permissions from the table, so a role can only
-- ever grant what exists.
create or replace function seed_roles(a_tenant uuid) returns void
    language plpgsql
as $$
declare
    seeded record;
    seeded_id uuid;
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

        select id into seeded_id from role
        where tenant_id = a_tenant and name = seeded.name;

        insert into role_grant (tenant_id, role_id, permission_id)
        select a_tenant, seeded_id, p.id
        from permission p
        where p.name = any(seeded.grants)
        on conflict do nothing;
    end loop;
end
$$;
