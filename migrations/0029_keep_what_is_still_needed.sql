-- How long a tenant's events are kept. Null means for ever, which is what
-- every tenant gets until somebody says otherwise: silently discarding what an
-- operator has not asked to lose is worse than a database that grows.
alter table tenant add column retention_days integer;

alter table tenant add constraint tenant_retention_is_a_span
    check (retention_days is null or retention_days between 1 and 3650);
