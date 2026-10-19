-- A route made before anybody was asked delivered everything ever recorded,
-- and keeps doing so.
alter table route
    add column events_recorded_from timestamptz not null default '-infinity';

-- When an event was recorded is read off its identifier, which the database
-- makes. The instant a request carries comes from the clock of whichever host
-- took it in, and comparing that with the database's own clock would let a host
-- running fast or slow decide what a route skips.
create function recorded_since(a_event uuid, a_since timestamptz) returns boolean
    language sql stable
as $$
    select uuid_extract_timestamp(a_event) >= a_since;
$$;

-- Planning and waking the dispatcher must agree on this, or the dispatcher
-- wakes for an event planning will never take, and does so every few
-- milliseconds.
create function wanted_by_a_route(a_provider varchar(100), a_event uuid) returns boolean
    language sql stable
as $$
    select exists (
        select 1 from route r
        join destination d on d.id = r.destination_id and d.enabled
        where r.provider = a_provider and recorded_since(a_event, r.events_recorded_from)
    );
$$;
