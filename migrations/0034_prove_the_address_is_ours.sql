-- Some providers will not accept a callback address until the address proves
-- it is ours: they send a GET carrying a token they were told and a value to
-- echo back, and deliver nothing until they get that value back. The token is
-- a secret like any other here, so this column holds the name of the
-- environment variable carrying it and never the value, the way secret_env
-- already does. Empty is the answer for every provider that asks for nothing,
-- and it leaves the address answering a GET exactly as it did before this
-- column existed.
alter table provider add column verify_token_env varchar(100) not null default '';
