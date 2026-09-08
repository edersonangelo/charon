-- Belonging to a tenant is a relationship, not a column on the person: someone
-- can be a viewer in one tenant and an owner in another, and sees one at a
-- time. The four roles Charon ships are its own and can be held in any tenant;
-- a tenant may still define roles of its own, which is what a tenant on the
-- role means. Null there means the role belongs to no tenant in particular.
alter table role alter column tenant_id drop not null;
alter table role drop constraint role_tenant_id_name_key;
create unique index role_name_key on role (tenant_id, name) nulls not distinct;

-- One copy of each shipped role, held by everyone, instead of one copy per
-- tenant. The first tenant's copy is promoted and the rest fold into it.
do $$
declare
    shipped record;
    keeper  uuid;
begin
    for shipped in select distinct name from role where built_in loop
        select id into keeper from role
        where built_in and name = shipped.name order by created_at limit 1;

        update role set tenant_id = null where id = keeper;
        update panel_user u set role_id = keeper
        where u.role_id in (select id from role where built_in and name = shipped.name and id <> keeper);
        update role_mapping m set role_id = keeper
        where m.role_id in (select id from role where built_in and name = shipped.name and id <> keeper);

        delete from role where built_in and name = shipped.name and id <> keeper;
    end loop;
end
$$;

-- No role recorded is not an absence of an answer, it is the answer: the least
-- a member can hold. Somebody in more than one tenant states a role for each,
-- or holds the least in each.
create table membership (
    user_id    uuid not null references panel_user (id) on delete cascade,
    tenant_id  uuid not null references tenant (id) on delete cascade,
    role_id    uuid references role (id),
    created_at timestamptz not null default now(),
    primary key (user_id, tenant_id)
);

create index membership_tenant_idx on membership (tenant_id);

insert into membership (user_id, tenant_id, role_id)
select u.id, coalesce(r.tenant_id, (select id from tenant order by created_at limit 1)), u.role_id
from panel_user u join role r on r.id = u.role_id;

alter table panel_user drop constraint panel_user_belongs_somewhere;
alter table panel_user drop column role_id;

-- A mapping says which tenant, because a role no longer carries one. One claim
-- can lead to several tenants, which is how somebody arrives holding more than
-- one place at once.
alter table role_mapping add column tenant_id uuid references tenant (id) on delete cascade;
update role_mapping m set tenant_id = (select id from tenant order by created_at limit 1);
alter table role_mapping alter column tenant_id set not null;
alter table role_mapping drop constraint role_mapping_pkey;
alter table role_mapping add primary key (method, claim, tenant_id);

-- A mapping that names no role says only where, and where without a role is
-- the least, the same as a membership with none.
alter table role_mapping alter column role_id drop not null;

-- A tenant's own roles stay its own; the ones Charon ships belong to nobody
-- and are visible from everywhere.
drop policy tenant_isolation on role;
create policy tenant_isolation on role
    using (tenant_id is null or tenant_id = current_tenant())
    with check (tenant_id = current_tenant());

drop policy tenant_isolation on role_grant;
create policy tenant_isolation on role_grant
    using (exists (select 1 from role r where r.id = role_grant.role_id
                   and (r.tenant_id is null or r.tenant_id = current_tenant())))
    with check (exists (select 1 from role r where r.id = role_grant.role_id
                        and r.tenant_id = current_tenant()));

-- Roles are no longer seeded per tenant, because they are not per tenant.
drop function seed_roles(uuid);
