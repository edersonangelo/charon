-- For somebody arriving from an identity provider, the token says where they
-- belong, and nothing else does. Keeping a note of which memberships came from
-- elsewhere only preserved what the token contradicts.
alter table membership drop column source;
drop table membership_source;
