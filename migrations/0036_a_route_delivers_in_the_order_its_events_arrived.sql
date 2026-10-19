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
    on delivery (destination_id, event_provider, event_id)
    where state = 'pending';

create index delivery_in_flight_idx
    on delivery (destination_id, event_provider)
    where state = 'pending' and leased_until is not null;

create index inbound_event_unplanned_by_provider_idx
    on inbound_event (provider, id)
    where planned_at is null;

-- The first delivery each route still owes, in the order its events were
-- recorded, for every route with nothing in flight: one in flight holds the
-- route even when a replay has put an older one ahead of it. A route is held while an earlier event of its provider is not
-- planned yet, because a dispatcher running beside this one may be about to
-- plan it; one whose signature was refused will never be delivered and holds
-- nothing. A dead delivery is no longer waiting, so it lets the next one go.
create function first_waiting_of_each_route() returns setof uuid
    language sql stable
as $$
    select head.id
    from (select distinct dl.destination_id, dl.event_provider
          from delivery dl
          where dl.state = 'pending') waiting
    cross join lateral (
        select dl.id, dl.event_id
        from delivery dl
        where dl.destination_id = waiting.destination_id
          and dl.event_provider = waiting.event_provider
          and dl.state = 'pending'
        order by dl.event_id
        limit 1
    ) head
    where not exists (
        select 1 from delivery flying
        where flying.state = 'pending'
          and flying.destination_id = waiting.destination_id
          and flying.event_provider = waiting.event_provider
          and flying.leased_until is not null and flying.leased_until > now()
    )
    and not exists (
        select 1
        from inbound_event unplanned
        join route r on r.provider = unplanned.provider
                    and r.destination_id = waiting.destination_id
        where unplanned.planned_at is null
          and unplanned.provider = waiting.event_provider
          and unplanned.id < head.event_id
          and (unplanned.signature in ('unchecked', 'valid') or unplanned.override_id is not null)
    );
$$;
