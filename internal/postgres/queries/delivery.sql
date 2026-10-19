-- name: CreateDestination :one
insert into destination (tenant_id, name, url, transport)
values ($1, $2, $3, $4)
returning *;

-- name: DestinationByName :one
select * from destination where tenant_id = $1 and name = $2;

-- An event's identifier carries its instant to the millisecond, so a route
-- starts on a millisecond too: what is recorded in the same one counts as after.
-- name: CreateRoute :one
insert into route (tenant_id, provider, destination_id, events_recorded_from)
values ($1, $2, $3,
        case when sqlc.arg(deliver_recorded)::boolean then '-infinity'::timestamptz
             else date_trunc('milliseconds', now()) end)
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
-- An event no route wants is left out: taking it would spend the batch on it
-- every round, and a tenant with enough of them would never plan again.
-- name: ClaimUnplannedEvents :many
select e.id, e.tenant_id, e.provider, e.signature,
       (e.override_id is not null)::boolean as overridden
from inbound_event e
where e.planned_at is null
  and ((e.signature in ('invalid', 'missing') and e.override_id is null)
       or wanted_by_a_route(e.provider, e.id))
order by e.id
limit $1
for update skip locked;

-- name: EnabledDestinationsForEvent :many
select d.id from route r
join destination d on d.id = r.destination_id
where r.tenant_id = sqlc.arg(tenant_id)
  and r.provider = sqlc.arg(provider)
  and recorded_since(sqlc.arg(event_id)::uuid, r.events_recorded_from)
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
  and recorded_since(e.id, r.events_recorded_from)
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

-- name: MarkEventPlanned :exec
update inbound_event set planned_at = now() where id = $1;

-- Switching a destination off pauses it. What is already waiting for it stays
-- waiting, keeping its attempts, instead of being spent against somewhere that
-- was deliberately taken out of service — which would leave a delivery dead by
-- the time it came back.
--
-- Only the first waiting delivery of a route is a candidate, so one in flight
-- or waiting out a retry holds the ones behind it. What decides it is checked
-- again on the locked row, which is how two dispatchers never take two from
-- one route.
-- name: ClaimDeliveries :many
update delivery
set leased_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::float)
where id in (
    select dl.id from delivery dl
    join destination d on d.id = dl.destination_id
    where dl.id in (select first_waiting_of_each_route())
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

-- Only the first waiting delivery of a route can go, so only it says when there
-- is work. Counting the ones behind it would wake the dispatcher over and over
-- while the first waits out a retry.
-- name: NextWorkAt :one
with next as (
    select least(
        (select min(greatest(dl.next_attempt_at, coalesce(dl.leased_until, dl.next_attempt_at)))
         from delivery dl
         join destination d on d.id = dl.destination_id
         where dl.id in (select first_waiting_of_each_route()) and d.enabled),
        (select case when exists (
                    select 1 from inbound_event e
                    where e.planned_at is null
                      and (e.signature in ('unchecked', 'valid')
                           or e.override_id is not null)
                      and wanted_by_a_route(e.provider, e.id)
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
