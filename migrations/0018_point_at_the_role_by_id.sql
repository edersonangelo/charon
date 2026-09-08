-- A column named for what it points at, pointing at that and nothing else. A
-- role already belongs to a tenant, so naming a role says which tenant without
-- carrying it again, and a role is named by its identifier like everything
-- else. What is left carrying tenant_id carries it because the tenant is what
-- scopes it, and points straight at the tenant.
alter table panel_user add column role_id uuid references role (id) on delete cascade;
update panel_user u set role_id = r.id
from role r where r.tenant_id = u.tenant_id and r.name = u.role;

alter table panel_user drop constraint panel_user_belongs_somewhere;
alter table panel_user drop constraint panel_user_role_fkey;
alter table panel_user drop column tenant_id;
alter table panel_user drop column role;
alter table panel_user add constraint panel_user_belongs_somewhere
    check ((role_id is null) = system_admin);

alter table role_mapping add column role_id uuid references role (id) on delete cascade;
update role_mapping m set role_id = r.id
from role r where r.tenant_id = m.tenant_id and r.name = m.role;

drop index role_mapping_tenant_idx;
alter table role_mapping drop column tenant_id;
alter table role_mapping drop column role;
alter table role_mapping alter column role_id set not null;

-- A grant is reached through the role, so the tenant it is confined to is the
-- role's, read where it actually lives instead of copied alongside.
drop policy tenant_isolation on role_grant;
drop index role_grant_tenant_idx;

alter table role_grant drop constraint role_grant_role_fkey;
alter table role_grant drop column tenant_id;
alter table role_grant add constraint role_grant_role_id_fkey
    foreign key (role_id) references role (id) on delete cascade;

alter table role drop constraint role_tenant_id_id_key;

create policy tenant_isolation on role_grant
    using (exists (select 1 from role r
                   where r.id = role_grant.role_id and r.tenant_id = current_tenant()))
    with check (exists (select 1 from role r
                        where r.id = role_grant.role_id and r.tenant_id = current_tenant()));

-- Seeding follows: a grant names the role, and the role carries the tenant.
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

        insert into role_grant (role_id, permission_id)
        select seeded_id, p.id from permission p where p.name = any(seeded.grants)
        on conflict do nothing;
    end loop;
end
$$;
