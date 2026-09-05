-- A value whose name already matches a tenant needs nothing written down. One
-- that does not — a directory naming its groups its own way — is pointed at a
-- tenant here. The convention is the rule and this is the exception, so an
-- empty table is a working deployment.
create table claim_placement (
    method     varchar(40)  not null references auth_method (name) on update cascade,
    value      varchar(200) not null,
    tenant_id  uuid         not null references tenant (id) on delete cascade,
    role_id    uuid         references role (id) on delete set null,
    created_at timestamptz  not null default now(),
    primary key (method, value, tenant_id)
);

create index claim_placement_tenant_idx on claim_placement (tenant_id);
