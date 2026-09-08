-- name: AddSigningSecret :exec
insert into signing_secret (tenant_id, destination_id, reference)
values ($1, $2, $3)
on conflict (destination_id, reference) do nothing;

-- name: RemoveSigningSecret :execrows
delete from signing_secret
where tenant_id = $1 and destination_id = $2 and reference = $3;

-- name: SigningSecretsFor :many
select reference from signing_secret
where destination_id = $1
order by created_at;

-- name: SigningSecrets :many
select d.name as destination, s.reference, s.created_at
from signing_secret s
join destination d on d.id = s.destination_id
where s.tenant_id = $1
order by d.name, s.created_at;

-- name: SigningSecretsEverywhere :many
select s.id, s.tenant_id, s.reference from signing_secret s;

-- name: RecordSigningCheck :exec
insert into signing_secret_check (signing_secret_id, tenant_id, readable, detail, checked_at)
values ($1, $2, $3, $4, now())
on conflict (signing_secret_id) do update
set readable = excluded.readable,
    detail = excluded.detail,
    checked_at = excluded.checked_at;

-- name: SigningFor :many
select s.reference, s.created_at, c.readable, coalesce(c.detail, '') as detail, c.checked_at
from signing_secret s
left join signing_secret_check c on c.signing_secret_id = s.id
where s.destination_id = $1
order by s.created_at;

-- name: DestinationsWithoutSigning :many
select d.id, d.name, count(r.id) as routes
from destination d
left join route r on r.destination_id = d.id
where d.tenant_id = $1 and d.enabled
  and not exists (select 1 from signing_secret s where s.destination_id = d.id)
group by d.id, d.name
having count(r.id) > 0
order by d.name;

-- name: NotifySigning :exec
select pg_notify('charon_signing', '');
