-- A role belongs to a tenant, so pointing at a role already says which tenant.
-- Carrying a second key to the tenant beside it states the same thing twice,
-- and in one place it stated something weaker: a grant could name a role of one
-- tenant while claiming to belong to another.
alter table role add constraint role_tenant_id_id_key unique (tenant_id, id);

alter table role_grant drop constraint role_grant_tenant_id_fkey;
alter table role_grant drop constraint role_grant_role_id_fkey;
alter table role_grant add constraint role_grant_role_fkey
    foreign key (tenant_id, role_id) references role (tenant_id, id)
    on update cascade on delete cascade;

alter table role_mapping drop constraint role_mapping_tenant_id_fkey;

-- An operator either belongs to a tenant as a role, or is a system account and
-- belongs to neither. Saying that outright is what lets the key to the role
-- cover the tenant as well: the two columns are never one without the other.
alter table panel_user drop constraint panel_user_belongs_somewhere;
alter table panel_user add constraint panel_user_belongs_somewhere
    check ((tenant_id is null) = system_admin and (role is null) = system_admin);

alter table panel_user drop constraint panel_user_tenant_id_fkey;
alter table panel_user drop constraint panel_user_role_fkey;
alter table panel_user add constraint panel_user_role_fkey
    foreign key (tenant_id, role) references role (tenant_id, name)
    on update cascade on delete cascade;
