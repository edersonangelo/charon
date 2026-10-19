-- Every claim looks for the first waiting delivery of each route. Carrying the
-- provider on the delivery lets that be read off one index, where joining every
-- waiting delivery to its event made each claim slower the longer a backlog.
alter table delivery add column event_provider varchar(100);

-- The rows already here belong to every tenant, and under forced row level
-- security this statement would be shown none of them.
alter table delivery      no force row level security;
alter table inbound_event no force row level security;
update delivery dl set event_provider = e.provider
from inbound_event e
where e.id = dl.event_id;
alter table delivery      force row level security;
alter table inbound_event force row level security;

alter table delivery alter column event_provider set not null;

create index delivery_first_waiting_idx
    on delivery (tenant_id, destination_id, event_provider, event_id)
    where state = 'pending';

create index delivery_in_flight_idx
    on delivery (tenant_id, destination_id, event_provider)
    where state = 'pending' and leased_until is not null;

-- The first delivery each route still owes, in the order its events were
-- recorded, for every route with nothing in flight: one in flight holds the
-- route even when a replay has put an older one ahead of it.
--
-- The routes are found by stepping through the index one route at a time, so
-- the work is the number of routes with something waiting, not the number of
-- deliveries waiting.
--
-- A route is held while an earlier event it wants is not planned yet, because a
-- dispatcher running beside this one may be about to plan it. One whose
-- signature was refused will never be delivered and holds nothing. A dead
-- delivery is no longer waiting, so it lets the next one go.
--
-- It is plpgsql so that it is planned on its own: written in SQL it was folded
-- into whichever query called it, and planned there as a scan of every waiting
-- delivery.
create function first_waiting_of_each_route() returns setof uuid
    language plpgsql stable rows 10
as $$
begin
    return query
    with recursive waiting (destination_id, event_provider) as (
        (select dl.destination_id, dl.event_provider
         from delivery dl
         where dl.tenant_id = current_tenant() and dl.state = 'pending'
         order by dl.destination_id, dl.event_provider
         limit 1)
        union all
        (select later.destination_id, later.event_provider
         from waiting w
         cross join lateral (
             select dl.destination_id, dl.event_provider
             from delivery dl
             where dl.tenant_id = current_tenant() and dl.state = 'pending'
               and (dl.destination_id, dl.event_provider) > (w.destination_id, w.event_provider)
             order by dl.destination_id, dl.event_provider
             limit 1
         ) later)
    )
    select head.id
    from waiting w
    cross join lateral (
        select dl.id, dl.event_id
        from delivery dl
        where dl.tenant_id = current_tenant() and dl.state = 'pending'
          and dl.destination_id = w.destination_id and dl.event_provider = w.event_provider
        order by dl.event_id
        limit 1
    ) head
    where not exists (
        select 1 from delivery flying
        where flying.tenant_id = current_tenant() and flying.state = 'pending'
          and flying.destination_id = w.destination_id and flying.event_provider = w.event_provider
          and flying.leased_until is not null and flying.leased_until > now()
    )
    -- Asked per route rather than joined, so it reads only the range between
    -- where the route starts and its first waiting delivery.
    and not (select exists (
        select 1
        from inbound_event unplanned
        where unplanned.tenant_id = current_tenant()
          and unplanned.provider = w.event_provider
          and unplanned.planned_at is null
          and unplanned.id >= (
              select first_id_recorded_at(r.events_recorded_from)
              from route r
              where r.destination_id = w.destination_id and r.provider = w.event_provider)
          and unplanned.id < head.event_id
          and (unplanned.signature in ('unchecked', 'valid') or unplanned.override_id is not null)
        order by unplanned.tenant_id, unplanned.provider, unplanned.id
    ));
end;
$$;
