-- A system administrator belongs to no tenant, and a role belongs to one, so
-- they hold neither. What they may do does not come from a role: it is the
-- standing itself. Everybody else must have both.
alter table panel_user alter column tenant_id drop not null;
alter table panel_user alter column role      drop not null;

alter table panel_user add constraint panel_user_belongs_somewhere
    check (system_admin or (tenant_id is not null and role is not null));

update panel_user set tenant_id = null, role = null where system_admin;
