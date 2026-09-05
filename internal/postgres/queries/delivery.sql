-- name: CreateDestination :one
insert into destination (tenant_id, name, url, transport)
values ($1, $2, $3, $4)
returning *;

-- name: DestinationByName :one
select * from destination where tenant_id = $1 and name = $2;

-- name: CreateRoute :exec
insert into route (tenant_id, provider, destination_id)
values ($1, $2, $3)
on conflict (tenant_id, provider, destination_id) do nothing;

-- name: ListRoutes :many
select r.provider, d.name, d.url, d.transport, d.enabled
from route r
join destination d on d.id = r.destination_id
where r.tenant_id = $1
order by r.provider, d.name;

-- A signature that was checked and failed is never delivered. One that was
-- never checked is: a provider with no verifier configured behaves as before.
-- name: ClaimUnplannedEvents :many
select id, tenant_id, provider, signature from inbound_event
where planned_at is null
order by received_at
limit $1
for update skip locked;

-- name: EnabledDestinationsForProvider :many
select d.id from route r
join destination d on d.id = r.destination_id
where r.tenant_id = $1 and r.provider = $2 and d.enabled;

-- name: CreateDelivery :exec
insert into delivery (tenant_id, event_id, destination_id, state, next_attempt_at)
values ($1, $2, $3, 'pending', now())
on conflict (event_id, destination_id) do nothing;

-- name: MarkEventPlanned :exec
update inbound_event set planned_at = now() where id = $1;

-- name: ClaimDeliveries :many
update delivery
set leased_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::float)
where id in (
    select id from delivery
    where state = 'pending'
      and next_attempt_at <= now()
      and (leased_until is null or leased_until <= now())
    order by next_attempt_at
    limit sqlc.arg(batch_size)
    for update skip locked
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

-- name: NextWorkAt :one
with next as (
    select least(
        (select min(next_attempt_at) from delivery where state = 'pending'),
        (select case when exists (
                    select 1 from inbound_event e
                    join route r on r.provider = e.provider
                    join destination d on d.id = r.destination_id and d.enabled
                    where e.planned_at is null
                      and e.signature in ('unchecked', 'valid')
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
