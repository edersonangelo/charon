-- What a destination says back is read and thrown away, and the status code is
-- kept as though it were the whole answer. Usually it is. It is not something
-- to rely on: a rejection carries a message, a validation failure carries which
-- field, and a queue carries the identifier it filed the thing under. Losing
-- that leaves an operator with a number and no idea what it meant.
--
-- Held verbatim, like a request body is, because what was said is what has to
-- be readable later. The content type comes with it: without it there is no
-- telling whether the bytes are worth showing as text at all.
alter table delivery_attempt add column response      bytea;
alter table delivery_attempt add column response_type varchar(120) not null default '';

-- Bounded when it is read, not here, so the limit is a deployment's to choose.
-- This says whether what is kept is all of it.
alter table delivery_attempt add column response_truncated boolean not null default false;
