-- A system administrator is not a role inside a tenant: it is a property of the
-- account, because the whole point is that it is not confined to one. It stays
-- a flag rather than a permission for the same reason — a permission is granted
-- by a role, and a role belongs to a tenant.
alter table panel_user add column system_admin boolean not null default false;

create index panel_user_system_admin_idx on panel_user (system_admin) where system_admin;
