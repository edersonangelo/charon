-- A route made before anybody was asked delivered everything ever recorded,
-- and keeps doing so.
alter table route
    add column events_received_from timestamptz not null default '-infinity';

-- Planning and waking the dispatcher must agree on this, or the dispatcher
-- wakes for an event planning will never take, and does so every few
-- milliseconds.
create function wanted_by_a_route(a_provider varchar(100), a_received_at timestamptz)
    returns boolean
    language sql stable
as $$
    select exists (
        select 1 from route r
        join destination d on d.id = r.destination_id and d.enabled
        where r.provider = a_provider and r.events_received_from <= a_received_at
    );
$$;
