-- The tenant of a row was defaulted to the one the first migration wrote out,
-- which stopped being that tenant the moment its identifier was generated like
-- every other. A default that points at a row nobody has is worse than none:
-- whose a row is has to be said, and every write says it.
alter table inbound_event    alter column tenant_id drop default;
alter table inbound_request  alter column tenant_id drop default;
alter table delivery         alter column tenant_id drop default;
alter table delivery_attempt alter column tenant_id drop default;
alter table destination      alter column tenant_id drop default;
alter table route            alter column tenant_id drop default;
alter table provider         alter column tenant_id drop default;
