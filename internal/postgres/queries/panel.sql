-- name: CreatePanelUser :one
insert into panel_user (email, password_hash)
values ($1, $2)
returning id;

-- name: CreateSystemAdmin :exec
insert into panel_user (email, password_hash, system_admin)
values ($1, $2, true);

-- name: SystemAdmins :many
select id, email, oidc_subject, created_at
from panel_user
where system_admin
order by email;

-- name: PanelUserByEmail :one
select * from panel_user where email = $1;

-- name: CountPanelUsers :one
select count(*) from panel_user;

-- name: CreatePanelSession :exec
insert into panel_session (token, user_id, expires_at)
values ($1, $2, $3);

-- name: PanelSessionUser :one
select u.id, u.email, u.system_admin
from panel_session s
join panel_user u on u.id = s.user_id
where s.token = $1 and s.expires_at > now();

-- name: DeletePanelSession :exec
delete from panel_session where token = $1;

-- name: DeleteExpiredPanelSessions :exec
delete from panel_session where expires_at <= now();

-- name: RecordDeliveryAttempt :exec
insert into delivery_attempt (tenant_id, delivery_id, attempt, round, status, error, duration_ms, signed_with, forced)
select d.tenant_id, $1, $2, d.replay_count, $3, $4, $5, $6,
       (select e.override_id is not null from inbound_event e where e.id = d.event_id)
from delivery d
where d.id = $1;

-- name: DeliveryAttempts :many
select round, attempt, attempted_at, status, error, duration_ms, signed_with, forced
from delivery_attempt
where delivery_id = $1 and tenant_id = $2
order by round desc, attempted_at desc;

-- The state counts only take deliveries whose destination is still routed and
-- enabled, so the numbers reconcile with the routes page. What is left over is
-- reported apart as history.
-- name: SearchEvents :many
select e.id, e.provider, e.path, e.received_at, e.body_size, e.signature,
       (e.planned_at is not null)::boolean as planned,
       (e.override_id is not null)::boolean as forced,
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
where e.tenant_id = sqlc.arg(tenant_id)
  and (sqlc.narg(provider)::text is null or e.provider = sqlc.narg(provider)::text)
  and (sqlc.narg(since)::timestamptz is null or e.received_at >= sqlc.narg(since)::timestamptz)
  and (sqlc.narg(until)::timestamptz is null or e.received_at <= sqlc.narg(until)::timestamptz)
  and (sqlc.narg(signature)::text is null or e.signature = sqlc.narg(signature)::text)
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
select distinct provider from inbound_event where tenant_id = $1 order by provider;

-- name: EventDetail :one
select e.id, e.provider, e.path, e.received_at, e.body_size, e.planned_at,
       e.signature, r.headers, r.body
from inbound_event e
join inbound_request r on r.event_id = e.id
where e.id = $1 and e.tenant_id = $2;

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
where d.event_id = $1 and d.tenant_id = $2
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
  and dl.tenant_id = $2
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
  and dl.tenant_id = $2
  and exists (
      select 1
      from inbound_event e
      join route r on r.provider = e.provider
      join destination dst on dst.id = r.destination_id and dst.enabled
      where e.id = dl.event_id and r.destination_id = dl.destination_id
  );

-- name: UnplanEvent :exec
update inbound_event set planned_at = null where id = $1 and tenant_id = $2;

-- name: DeliveryStateTotals :many
select state, count(*) as total from delivery where tenant_id = $1 group by state;

-- name: PanelUserBySubject :one
select * from panel_user where oidc_subject = $1;

-- name: CreateSSOUser :one
insert into panel_user (email, oidc_subject)
values ($1, $2)
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
where e.tenant_id = $1
  and not exists (
      select 1 from route r
      where r.tenant_id = e.tenant_id and r.provider = e.provider
  )
group by e.provider
order by max(e.received_at) desc;

-- name: DetailedRoutes :many
select r.id, r.provider, d.id as destination_id, d.name, d.url, d.transport, d.enabled,
       (select count(*) from delivery dl where dl.destination_id = d.id)::bigint as deliveries
from route r
join destination d on d.id = r.destination_id
where r.tenant_id = $1
order by r.provider, d.name;

-- name: UpdateDestination :exec
update destination set url = $2, enabled = $3 where id = $1 and tenant_id = $4;

-- name: DeleteRoute :exec
delete from route where id = $1 and tenant_id = $2;

-- name: RouteByID :one
select r.id, r.provider, d.id as destination_id, d.name, d.enabled
from route r join destination d on d.id = r.destination_id
where r.id = $1 and r.tenant_id = $2;

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
  and dl.tenant_id = $2
  and exists (
      select 1
      from inbound_event e
      join route r on r.provider = e.provider
      join destination dst on dst.id = r.destination_id and dst.enabled
      where e.id = dl.event_id and r.destination_id = dl.destination_id
  );

-- name: UnplanProvider :execrows
update inbound_event set planned_at = null where provider = $1 and tenant_id = $2;

-- name: ProviderAlreadyRoutedTo :one
select exists (
    select 1 from route r
    join destination d on d.id = r.destination_id
    where r.tenant_id = $1 and r.provider = $2 and d.url = $3
)::boolean as taken;

-- name: CountTenants :one
select count(*) from tenant;

-- name: Roles :many
select r.name, r.description, r.built_in,
       coalesce(array_agg(p.name order by p.name)
                filter (where p.name is not null), '{}')::text[] as grants
from role r
left join role_grant g on g.role_id = r.id
left join permission p on p.id = g.permission_id
where r.tenant_id is null or r.tenant_id = $1
group by r.id, r.name, r.description, r.built_in
order by r.built_in desc, r.name;

-- name: Role :one
select r.name, r.description, r.built_in,
       coalesce(array_agg(p.name order by p.name)
                filter (where p.name is not null), '{}')::text[] as grants
from role r
left join role_grant g on g.role_id = r.id
left join permission p on p.id = g.permission_id
where (r.tenant_id is null or r.tenant_id = $1) and r.name = $2
group by r.id, r.name, r.description, r.built_in
-- A tenant may define a role of its own under a name Charon also ships, and
-- changing a shipped role is how that happens. Its own wins, said here rather
-- than left to whichever row the database hands back first: deciding what
-- somebody may do cannot depend on that.
order by (r.tenant_id is null)
limit 1;

-- name: SetRole :one
insert into role (tenant_id, name, description)
values ($1, $2, $3)
on conflict (tenant_id, name) do update set description = excluded.description
returning id;

-- name: ClearRoleGrants :exec
delete from role_grant where role_id = $1;

-- name: GrantPermission :exec
insert into role_grant (role_id, permission_id)
select $1, p.id from permission p where p.name = $2
on conflict do nothing;

-- name: Permissions :many
select name, description from permission order by name;

-- name: DeleteRole :execrows
delete from role where tenant_id = $1 and name = $2 and not built_in;

-- name: PanelUsers :many
select u.id, u.email, r.name as role, u.oidc_subject, m.created_at
from membership m
join panel_user u on u.id = m.user_id
left join role r on r.id = m.role_id
where m.tenant_id = $1
order by u.email;

-- name: UpdatePanelUserRole :execrows
update membership set role_id = $3 where user_id = $1 and tenant_id = $2;

-- name: DeletePanelUser :execrows
delete from membership where user_id = $1 and tenant_id = $2;

-- Becoming one means leaving the role behind, and with it the tenant the role
-- belonged to; giving it up means being given a role again.
-- name: SetSystemAdmin :execrows
update panel_user set system_admin = true where id = $1;

-- name: LeaveEveryTenant :exec
delete from membership where user_id = $1;

-- name: ClearSystemAdmin :execrows
update panel_user set system_admin = false where id = $1;

-- name: SetSystemAdminByEmail :execrows
update panel_user set system_admin = true where email = $1;

-- name: LeaveEveryTenantByEmail :exec
delete from membership where user_id in (select id from panel_user where email = $1);

-- name: ClearSystemAdminByEmail :execrows
update panel_user set system_admin = false where email = $1;

-- name: CountSystemAdmins :one
select count(*) from panel_user where system_admin;

-- name: RoleIDByName :one
select id from role where (tenant_id is null or tenant_id = $1) and name = $2;

-- name: RoleByID :one
select tenant_id, name from role where id = $1;

-- name: RegisterTransport :exec
insert into transport (name) values ($1) on conflict (name) do nothing;

-- name: RegisterVerifier :exec
insert into verifier (name) values ($1) on conflict (name) do nothing;

-- name: RegisterAuthMethod :exec
insert into auth_method (name) values ($1) on conflict (name) do nothing;

-- name: JoinTenant :exec
insert into membership (user_id, tenant_id, role_id)
values ($1, $2, $3)
on conflict (user_id, tenant_id) do update set role_id = excluded.role_id;

-- A value that names only a tenant leaves the role to whoever decides it here,
-- so arriving again does not undo it.
-- name: JoinFromProvider :exec
insert into membership (user_id, tenant_id)
values ($1, $2)
on conflict (user_id, tenant_id) do nothing;

-- A value that names the role too makes the token the truth about it as well.
-- name: JoinFromProviderAs :exec
insert into membership (user_id, tenant_id, role_id)
values ($1, $2, $3)
on conflict (user_id, tenant_id) do update set role_id = excluded.role_id;

-- name: LeaveTenantsNoLongerNamed :exec
delete from membership where user_id = $1 and tenant_id <> all($2::uuid[]);

-- name: Memberships :many
select m.tenant_id, t.slug, t.name as tenant_name, coalesce(r.name, '') as role, m.created_at
from membership m
join tenant t on t.id = m.tenant_id
left join role r on r.id = m.role_id
where m.user_id = $1
order by t.slug;

-- name: MembershipIn :one
select coalesce(r.name, '') as role
from membership m
left join role r on r.id = m.role_id
where m.user_id = $1 and m.tenant_id = $2;

-- name: TenantsNamed :many
select id, slug from tenant where slug = any($1::varchar[]);

-- name: PlacementsPointedAt :many
select p.value, p.tenant_id, coalesce(r.name, '') as role
from claim_placement p
left join role r on r.id = p.role_id
where p.method = $1 and p.value = any($2::varchar[]);

-- name: PointValueAt :exec
insert into claim_placement (method, value, tenant_id, role_id)
values ($1, $2, $3, $4)
on conflict (method, value, tenant_id) do update set role_id = excluded.role_id;

-- name: StopPointingValue :exec
delete from claim_placement where method = $1 and value = $2 and tenant_id = $3;

-- name: ValuesPointedAtTenant :many
select p.method, p.value, coalesce(r.name, '') as role
from claim_placement p
left join role r on r.id = p.role_id
where p.tenant_id = $1
order by p.method, p.value;

-- name: RegisterPermission :exec
insert into permission (name, description) values ($1, $2)
on conflict (name) do update set description = excluded.description;

-- name: RegisterDeliveryState :exec
insert into delivery_state (name, description) values ($1, $2)
on conflict (name) do nothing;

-- name: RegisterSignatureState :exec
insert into signature_state (name, description) values ($1, $2)
on conflict (name) do nothing;

-- A role Charon ships belongs to no tenant and is held in any. Its description
-- is left alone once it exists, because a deployment is allowed to change it.
-- name: RegisterShippedRole :one
insert into role (tenant_id, name, description, built_in)
values (null, $1, $2, true)
on conflict (tenant_id, name) do update set built_in = true
returning id;

-- name: RegisterShippedGrant :exec
insert into role_grant (role_id, permission_id)
select $1, p.id from permission p where p.name = $2
on conflict do nothing;

-- name: RoleHasAnyGrant :one
select exists (select 1 from role_grant where role_id = $1);

-- name: RecordOverride :one
insert into signature_override (tenant_id, reason, decided_by)
values ($1, $2, $3)
returning id;

-- An event is overruled once. Saying so twice is the same decision, not a new
-- one, and the first is the one that let it out.
-- name: OverrideEvents :execrows
update inbound_event
set override_id = $1, planned_at = null
where tenant_id = $2
  and id = any(sqlc.arg(ids)::uuid[])
  and override_id is null
  and signature in ('invalid', 'missing');

-- name: OverruleOnEvent :one
select o.reason, o.decided_at, u.email as decided_by
from inbound_event e
join signature_override o on o.id = e.override_id
join panel_user u on u.id = o.decided_by
where e.id = $1 and e.tenant_id = $2;

-- name: EventsUnderOverride :many
select id from inbound_event where override_id = $1 order by received_at;
