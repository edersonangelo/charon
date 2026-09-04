-- name: CreatePanelUser :exec
insert into panel_user (id, email, password_hash)
values ($1, $2, $3);

-- name: PanelUserByEmail :one
select * from panel_user where email = $1;

-- name: CountPanelUsers :one
select count(*) from panel_user;

-- name: CreatePanelSession :exec
insert into panel_session (token, user_id, expires_at)
values ($1, $2, $3);

-- name: PanelSessionUser :one
select u.id, u.email
from panel_session s
join panel_user u on u.id = s.user_id
where s.token = $1 and s.expires_at > now();

-- name: DeletePanelSession :exec
delete from panel_session where token = $1;

-- name: DeleteExpiredPanelSessions :exec
delete from panel_session where expires_at <= now();

-- name: RecordDeliveryAttempt :exec
insert into delivery_attempt (id, delivery_id, attempt, round, status, error, duration_ms)
select $1, $2, $3, d.replay_count, $4, $5, $6
from delivery d
where d.id = $2;

-- name: DeliveryAttempts :many
select round, attempt, attempted_at, status, error, duration_ms
from delivery_attempt
where delivery_id = $1
order by round desc, attempted_at desc;

-- The state counts only take deliveries whose destination is still routed and
-- enabled, so the numbers reconcile with the routes page. What is left over is
-- reported apart as history.
-- name: SearchEvents :many
select e.id, e.provider, e.path, e.received_at, e.body_size,
       (e.planned_at is not null)::boolean as planned,
       coalesce((select count(*) from delivery d where d.event_id = e.id
                 and routed(e.provider, d.destination_id)), 0)::bigint as deliveries,
       coalesce((select count(*) from delivery d where d.event_id = e.id and d.state = 'delivered'
                 and routed(e.provider, d.destination_id)), 0)::bigint as delivered,
       coalesce((select count(*) from delivery d where d.event_id = e.id and d.state = 'dead'
                 and routed(e.provider, d.destination_id)), 0)::bigint as dead,
       coalesce((select count(*) from delivery d where d.event_id = e.id and d.state = 'pending'
                 and routed(e.provider, d.destination_id)), 0)::bigint as pending,
       coalesce((select count(*) from delivery d where d.event_id = e.id
                 and not routed(e.provider, d.destination_id)), 0)::bigint as unrouted,
       coalesce((select max(d.attempts) from delivery d where d.event_id = e.id
                 and routed(e.provider, d.destination_id)), 0)::int as attempts
from inbound_event e
where (sqlc.narg(provider)::text is null or e.provider = sqlc.narg(provider)::text)
  and (sqlc.narg(since)::timestamptz is null or e.received_at >= sqlc.narg(since)::timestamptz)
  and (sqlc.narg(until)::timestamptz is null or e.received_at <= sqlc.narg(until)::timestamptz)
  and (
       sqlc.narg(state)::text is null
       or (sqlc.narg(state)::text = 'unrouted' and not exists (
               select 1 from delivery d
               where d.event_id = e.id and routed(e.provider, d.destination_id)))
       or (sqlc.narg(state)::text <> 'unrouted' and exists (
               select 1 from delivery d
               where d.event_id = e.id
                 and d.state = sqlc.narg(state)::text
                 and routed(e.provider, d.destination_id)))
  )
  and (sqlc.narg(search)::text is null
       or e.id::text = sqlc.narg(search)::text
       or position(sqlc.narg(search)::text in convert_from(
              (select r.body from inbound_request r where r.event_id = e.id), 'UTF8')) > 0)
order by e.received_at desc
limit sqlc.arg(page_size) offset sqlc.arg(page_offset);

-- name: DistinctProviders :many
select distinct provider from inbound_event order by provider;

-- name: EventDetail :one
select e.id, e.provider, e.path, e.received_at, e.body_size, e.planned_at,
       r.headers, r.body
from inbound_event e
join inbound_request r on r.event_id = e.id
where e.id = $1;

-- name: EventDeliveries :many
select d.id, dest.name as destination, dest.url, d.state, d.attempts,
       d.next_attempt_at, d.last_status, d.last_error, d.delivered_at,
       d.replay_count, d.replayed_at,
       exists (
           select 1
           from inbound_event e
           join route r on r.provider = e.provider
           join destination active on active.id = r.destination_id and active.enabled
           where e.id = d.event_id and r.destination_id = d.destination_id
       )::boolean as routed
from delivery d
join destination dest on dest.id = d.destination_id
where d.event_id = $1
order by dest.name;

-- A replay only reopens deliveries whose destination is still routed for this
-- event's provider and still enabled. Anything else is history: the routes page
-- is the truth about where events go.

-- name: ReplayDelivery :execrows
update delivery as dl
set state = 'pending',
    attempts = 0,
    next_attempt_at = now(),
    leased_until = null,
    last_status = null,
    last_error = null,
    delivered_at = null,
    replay_count = replay_count + 1,
    replayed_at = now()
where dl.id = $1
  and exists (
      select 1
      from inbound_event e
      join route r on r.provider = e.provider
      join destination dst on dst.id = r.destination_id and dst.enabled
      where e.id = dl.event_id and r.destination_id = dl.destination_id
  );

-- name: ReplayEvent :execrows
update delivery as dl
set state = 'pending',
    attempts = 0,
    next_attempt_at = now(),
    leased_until = null,
    last_status = null,
    last_error = null,
    delivered_at = null,
    replay_count = replay_count + 1,
    replayed_at = now()
where dl.event_id = $1
  and exists (
      select 1
      from inbound_event e
      join route r on r.provider = e.provider
      join destination dst on dst.id = r.destination_id and dst.enabled
      where e.id = dl.event_id and r.destination_id = dl.destination_id
  );

-- name: UnplanEvent :exec
update inbound_event set planned_at = null where id = $1;

-- name: DeliveryStateTotals :many
select state, count(*) as total from delivery group by state;

-- name: PanelUserBySubject :one
select * from panel_user where oidc_subject = $1;

-- name: CreateSSOUser :one
insert into panel_user (id, email, oidc_subject)
values ($1, $2, $3)
returning *;

-- name: LinkSubjectToUser :one
update panel_user set oidc_subject = $2 where email = $1 returning *;

-- name: NotifyPanel :exec
select pg_notify('charon_panel', '');

-- name: UnroutedProviders :many
select e.provider,
       count(*)::bigint    as events,
       max(e.received_at)::timestamptz as last_received
from inbound_event e
where not exists (select 1 from route r where r.provider = e.provider)
group by e.provider
order by max(e.received_at) desc;

-- name: DetailedRoutes :many
select r.id, r.provider, d.id as destination_id, d.name, d.url, d.enabled,
       (select count(*) from delivery dl where dl.destination_id = d.id)::bigint as deliveries
from route r
join destination d on d.id = r.destination_id
order by r.provider, d.name;

-- name: UpdateDestination :exec
update destination set url = $2, enabled = $3 where id = $1;

-- name: DeleteRoute :exec
delete from route where id = $1;

-- name: RouteByID :one
select r.id, r.provider, d.id as destination_id, d.name, d.enabled
from route r join destination d on d.id = r.destination_id
where r.id = $1;

-- name: ReplayDestination :execrows
update delivery as dl
set state = 'pending',
    attempts = 0,
    next_attempt_at = now(),
    leased_until = null,
    last_status = null,
    last_error = null,
    delivered_at = null,
    replay_count = replay_count + 1,
    replayed_at = now()
where dl.destination_id = $1
  and exists (
      select 1
      from inbound_event e
      join route r on r.provider = e.provider
      join destination dst on dst.id = r.destination_id and dst.enabled
      where e.id = dl.event_id and r.destination_id = dl.destination_id
  );

-- name: UnplanProvider :execrows
update inbound_event set planned_at = null where provider = $1;

-- name: ProviderAlreadyRoutedTo :one
select exists (
    select 1 from route r
    join destination d on d.id = r.destination_id
    where r.provider = $1 and d.url = $2
)::boolean as taken;
