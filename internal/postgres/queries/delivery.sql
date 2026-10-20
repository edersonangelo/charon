-- name: CreateDestination :one
insert into destination (tenant_id, name, url, transport)
values ($1, $2, $3, $4)
returning *;

-- name: DestinationByName :one
select * from destination where tenant_id = $1 and name = $2;

-- An event's identifier carries its instant to the millisecond, so a route
-- starts on a millisecond too: what is recorded in the same one counts as after.
-- Catching up ends on the millisecond after, so nothing recorded alongside the
-- choice is left out of the order.
-- name: CreateRoute :one
insert into route (tenant_id, provider, destination_id, events_recorded_from, catching_up_to)
values ($1, $2, $3,
        case when sqlc.arg(deliver_recorded)::boolean then '-infinity'::timestamptz
             else date_trunc('milliseconds', now()) end,
        case when sqlc.arg(deliver_recorded)::boolean
             then date_trunc('milliseconds', now()) + interval '1 millisecond' end)
on conflict (tenant_id, provider, destination_id) do nothing
returning id;

-- name: ListRoutes :many
select r.provider, d.name, d.url, d.transport, d.enabled,
       (select count(*) from signing_secret s where s.destination_id = d.id) as signed
from route r
join destination d on d.id = r.destination_id
where r.tenant_id = $1
order by r.provider, d.name;

-- A signature that was checked and failed is never delivered, unless somebody
-- overruled that deliberately and said why. One that was never checked is
-- delivered: a provider with no verifier configured behaves as before.
--
-- Only what can be settled now is read: what a route wants, from where that
-- route starts, and what was refused. An event no route wants, or one a route
-- chose to skip, is never read again, so however many of them pile up they
-- cost planning nothing.
-- name: ClaimUnplannedEvents :many
select e.id, e.tenant_id, e.provider, e.signature,
       (e.override_id is not null)::boolean as overridden
from inbound_event e
where e.planned_at is null
  and e.id in (
      (select refused.id
       from inbound_event refused
       where refused.tenant_id = current_tenant() and refused.planned_at is null
         and refused.signature in ('invalid', 'missing') and refused.override_id is null
       order by refused.id
       limit sqlc.arg(batch_size))
      union
      (select wanted.id
       from route r
       join destination d on d.id = r.destination_id and d.enabled
       cross join lateral (
           select unplanned.id
           from inbound_event unplanned
           where unplanned.tenant_id = current_tenant() and unplanned.provider = r.provider
             and unplanned.planned_at is null
             and unplanned.id >= first_id_recorded_at(r.events_recorded_from)
           order by unplanned.id
           limit sqlc.arg(batch_size)
       ) wanted)
  )
order by e.id
limit sqlc.arg(batch_size)
for update of e skip locked;

-- name: EnabledDestinationsForEvent :many
select d.id from route r
join destination d on d.id = r.destination_id
where r.tenant_id = sqlc.arg(tenant_id)
  and r.provider = sqlc.arg(provider)
  and sqlc.arg(event_id)::uuid >= first_id_recorded_at(r.events_recorded_from)
  and d.enabled;

-- name: CreateDelivery :exec
insert into delivery (tenant_id, event_id, destination_id, event_provider, state, next_attempt_at)
values ($1, $2, $3, $4, 'pending', now())
on conflict (event_id, destination_id) do nothing;

-- What a destination is owed from before it could be reached: events already
-- planned without it, because its route did not exist yet or it was switched
-- off. They are given their deliveries here rather than planned again, because
-- planning them again would hold every other route of the provider until the
-- whole history had been through planning once more.
-- name: DeliverWhatWasRecordedTo :execrows
insert into delivery (tenant_id, event_id, destination_id, event_provider, state, next_attempt_at)
select e.tenant_id, e.id, r.destination_id, e.provider, 'pending', now()
from route r
join inbound_event e on e.tenant_id = r.tenant_id and e.provider = r.provider
where r.tenant_id = sqlc.arg(tenant_id)
  and r.destination_id = sqlc.arg(destination_id)
  and (sqlc.narg(provider)::varchar is null or r.provider = sqlc.narg(provider)::varchar)
  and e.planned_at is not null
  and (e.signature in ('unchecked', 'valid') or e.override_id is not null)
  and e.id >= first_id_recorded_at(r.events_recorded_from)
on conflict (event_id, destination_id) do nothing;

-- Planning reads which routes an event goes to and gives it its deliveries in
-- one transaction, and a route added or switched back on gives deliveries to
-- what was planned before it. Each waits for the other, or an event planned
-- while a route appeared would be given to neither. Planning shares the hold:
-- dispatchers running side by side do not wait for each other.
-- name: HoldRoutesForPlanning :exec
select pg_advisory_xact_lock_shared(2, hashtext(sqlc.arg(tenant_id)::uuid::text));

-- name: HoldRoutesForChange :exec
select pg_advisory_xact_lock(2, hashtext(sqlc.arg(tenant_id)::uuid::text));

-- Somebody gave these routes a past to deliver: what waits for the destination
-- now goes in order, and what arrives meanwhile waits behind it.
-- name: CatchUpRoutesOfDestination :exec
update route
set catching_up_to = date_trunc('milliseconds', now()) + interval '1 millisecond'
where tenant_id = sqlc.arg(tenant_id) and destination_id = sqlc.arg(destination_id);

-- A route has caught up once nothing from before the instant it catches up to
-- still waits: not delivered, and not yet planned either. From then on it
-- delivers side by side, and an old event replayed later goes on its own
-- instead of putting the route back in order. What counts as wanted and not
-- yet planned is what deliveries_free_to_go counts.
-- name: MarkRoutesCaughtUp :exec
update route r
set catching_up_to = null
where r.tenant_id = current_tenant()
  and r.catching_up_to is not null
  and not exists (
      select 1 from delivery dl
      where dl.tenant_id = current_tenant() and dl.state = 'pending'
        and dl.destination_id = r.destination_id and dl.event_provider = r.provider
        and dl.event_id < first_id_recorded_at(r.catching_up_to)
  )
  and not exists (
      select 1 from inbound_event unplanned
      where unplanned.tenant_id = current_tenant()
        and unplanned.provider = r.provider
        and unplanned.planned_at is null
        and unplanned.id >= first_id_recorded_at(r.events_recorded_from)
        and unplanned.id < first_id_recorded_at(r.catching_up_to)
        and (unplanned.signature in ('unchecked', 'valid') or unplanned.override_id is not null)
  );

-- name: MarkEventPlanned :exec
update inbound_event set planned_at = now() where id = $1;

-- Switching a destination off pauses it. What is already waiting for it stays
-- waiting, keeping its attempts, instead of being spent against somewhere that
-- was deliberately taken out of service — which would leave a delivery dead by
-- the time it came back.
--
-- A route catching up offers only its first waiting delivery, so one in flight
-- or waiting out a retry holds the ones behind it. What decides it is checked
-- again on the locked row, which is how two dispatchers never take two from a
-- route catching up.
-- name: ClaimDeliveries :many
update delivery
set leased_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::float)
where id in (
    select dl.id
    from delivery dl
    join destination d on d.id = dl.destination_id
    where dl.id = any (array(select deliveries_free_to_go(now(), sqlc.arg(batch_size)::int)))
      and dl.state = 'pending'
      and dl.next_attempt_at <= now()
      and (dl.leased_until is null or dl.leased_until <= now())
      and d.enabled
    order by dl.next_attempt_at
    limit sqlc.arg(batch_size)
    for update of dl skip locked
)
returning id, event_id, destination_id, attempts, replay_count;

-- name: DeliveryTarget :one
select d.url, d.transport, e.provider, e.path, r.headers, r.body
from delivery dl
join inbound_event e on e.id = dl.event_id
join inbound_request r on r.event_id = dl.event_id
join destination d on d.id = dl.destination_id
where dl.id = $1;

-- name: MarkDelivered :exec
update delivery
set state = 'delivered',
    delivered_at = now(),
    attempts = attempts + 1,
    last_status = $2,
    last_error = null,
    leased_until = null
where id = $1;

-- name: MarkFailed :exec
update delivery
set attempts = attempts + 1,
    next_attempt_at = $2,
    last_status = $3,
    last_error = $4,
    leased_until = null,
    state = case when attempts + 1 >= sqlc.arg(max_attempts)::int then 'dead' else 'pending' end
where id = $1;

-- name: OldestPendingAge :one
select coalesce(extract(epoch from now() - min(created_at)), 0)::float as seconds
from delivery where state = 'pending';

-- name: NotifyWork :exec
select pg_notify('charon_work', '');

-- Only what may go says when there is work. Counting what waits behind the
-- first delivery of a route catching up would wake the dispatcher over and over
-- while the first waits out a retry.
-- name: NextWorkAt :one
with next as (
    select least(
        (select min(dl.next_attempt_at)
         from delivery dl
         join destination d on d.id = dl.destination_id and d.enabled
         where dl.id = any (array(select deliveries_free_to_go('infinity', 1)))),
        -- What is in flight holds a route catching up until the attempt is
        -- recorded, and is claimed again when its lease runs out because the
        -- worker died.
        (select min(flying.leased_until)
         from delivery flying
         join destination d on d.id = flying.destination_id and d.enabled
         where flying.tenant_id = current_tenant() and flying.state = 'pending'
           and flying.leased_until is not null and flying.leased_until > now()),
        (select case when exists (
                    select 1 from inbound_event refused
                    where refused.tenant_id = current_tenant() and refused.planned_at is null
                      and refused.signature in ('invalid', 'missing') and refused.override_id is null
                ) or exists (
                    select 1
                    from route r
                    join destination d on d.id = r.destination_id and d.enabled
                    cross join lateral (
                        select 1
                        from inbound_event unplanned
                        where unplanned.tenant_id = current_tenant()
                          and unplanned.provider = r.provider
                          and unplanned.planned_at is null
                          and unplanned.id >= first_id_recorded_at(r.events_recorded_from)
                        limit 1
                    ) wanted
                ) then now() end)
    ) as at
)
select coalesce(at, now())::timestamptz as at,
       (at is not null)::boolean       as found
from next;

-- name: CountEventsAwaitingRoute :one
select count(*) from inbound_event e
where e.tenant_id = $1
  and e.planned_at is null
  and not exists (
      select 1 from route r
      join destination d on d.id = r.destination_id and d.enabled
      where r.provider = e.provider
  );
