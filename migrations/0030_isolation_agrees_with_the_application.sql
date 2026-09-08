-- A role that belongs to no tenant is readable from everywhere and was
-- writable from nowhere: the reading side of both policies admits a null
-- tenant and the writing side does not, so inserting one asks whether
-- null = current_tenant(), which is null, which fails the check.
--
-- Nothing noticed because a superuser is not subject to row level security at
-- all, and the database the compose file starts makes the application one.
-- Every ordinary deployment has an ordinary role, and on those the roles
-- Charon ships could not be written down.
drop policy tenant_isolation on role;
create policy tenant_isolation on role
    using (tenant_id is null or tenant_id = current_tenant())
    with check (tenant_id is null or tenant_id = current_tenant());

drop policy tenant_isolation on role_grant;
create policy tenant_isolation on role_grant
    using (exists (select 1 from role r where r.id = role_grant.role_id
                   and (r.tenant_id is null or r.tenant_id = current_tenant())))
    with check (exists (select 1 from role r where r.id = role_grant.role_id
                        and (r.tenant_id is null or r.tenant_id = current_tenant())));

-- The application and the database disagreed about what no tenant means. A
-- query whose context names none acts for the tenant every deployment has, and
-- says so by passing that identifier; the connection carrying it said nothing
-- at all, so the policy compared the row against null and refused the write.
--
-- One rule, in one place. This is a hardening layer, not the gate: what it has
-- to guarantee is that a query which names a tenant cannot reach another one,
-- and it still does. What it deliberately does not do is turn a query that
-- named none into an empty answer, because an empty answer is the silent
-- failure that hid this whole class of mistake in the first place.
create or replace function current_tenant() returns uuid
    language sql
    stable
as $$
    select coalesce(
        nullif(current_setting('charon.tenant', true), '')::uuid,
        (select id from tenant where slug = 'default')
    );
$$;

-- The migration that made the shipped roles belong to no tenant read the table
-- it was promoting, before the policy allowing a null tenant existed. On an
-- ordinary role that read matched nothing, so nothing was promoted and every
-- tenant kept its own copy of viewer, operator, admin and owner.
--
-- Finishing it here. The forcing is lifted for the length of this statement
-- because folding rows across tenants is exactly the work a per-tenant policy
-- is there to prevent, and the owner of a table may do that to its own; the
-- policies themselves are left as they are.
alter table role       no force row level security;
alter table role_grant no force row level security;

do $$
declare
    shipped record;
    keeper  uuid;
begin
    for shipped in select distinct name from role where built_in loop
        select id into keeper from role
        where built_in and name = shipped.name
        order by (tenant_id is null) desc, created_at
        limit 1;

        update role set tenant_id = null where id = keeper;

        update membership m set role_id = keeper
        where m.role_id in (select id from role
                            where built_in and name = shipped.name and id <> keeper);
        update claim_placement c set role_id = keeper
        where c.role_id in (select id from role
                            where built_in and name = shipped.name and id <> keeper);

        insert into role_grant (role_id, permission_id)
        select keeper, g.permission_id from role_grant g
        join role r on r.id = g.role_id
        where r.built_in and r.name = shipped.name and r.id <> keeper
        on conflict do nothing;

        delete from role where built_in and name = shipped.name and id <> keeper;
    end loop;
end
$$;

alter table role       force row level security;
alter table role_grant force row level security;
