-- Which kind of destination this is. Deliberately without a check constraint:
-- the set of transports is open, and a kind nobody registered is refused at
-- delivery time with a recorded reason rather than rejected here.
alter table destination add column transport text not null default 'http';
