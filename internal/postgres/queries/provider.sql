-- name: SetProvider :exec
insert into provider (
    tenant_id, name, verifier, secret_env, signature_header, tolerance_seconds,
    scheme, algorithm, encoding, timestamp_key, signature_key, verify_token_env
) values (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
on conflict (tenant_id, name) do update
set verifier = excluded.verifier,
    secret_env = excluded.secret_env,
    signature_header = excluded.signature_header,
    tolerance_seconds = excluded.tolerance_seconds,
    scheme = excluded.scheme,
    algorithm = excluded.algorithm,
    encoding = excluded.encoding,
    timestamp_key = excluded.timestamp_key,
    signature_key = excluded.signature_key,
    verify_token_env = excluded.verify_token_env;

-- name: Providers :many
select name, verifier, secret_env, signature_header, tolerance_seconds,
       scheme, algorithm, encoding, timestamp_key, signature_key, verify_token_env
from provider where tenant_id = $1 order by name;

-- name: DeleteProvider :exec
delete from provider where tenant_id = $1 and name = $2;

-- name: MarkSignature :exec
update inbound_event set signature = $2 where id = $1;

-- name: NotifyProviders :exec
select pg_notify('charon_provider', '');

-- name: UnverifiedEvents :many
select e.id, e.provider, r.headers, r.body
from inbound_event e
join inbound_request r on r.event_id = e.id
where e.tenant_id = $1 and e.provider = $2
  and e.signature in ('invalid', 'missing', 'unchecked')
  -- Something already handed over cannot be taken back, so it keeps the answer
  -- it went out with. Saying now that it was never signed would claim it had
  -- been held, and it was not.
  and not exists (select 1 from delivery d
                  where d.event_id = e.id and d.state = 'delivered')
order by e.received_at
limit $3;

-- name: MarkSignatureAndReopen :exec
update inbound_event set signature = $2, planned_at = null where id = $1;

-- name: SignatureTotals :many
select signature, count(*)::bigint as total from inbound_event group by signature;

-- name: RefusedByProvider :many
select provider, count(*)::bigint as refused
from inbound_event
where tenant_id = $1 and signature in ('invalid', 'missing')
group by provider;

-- name: KnownProviders :many
select p.name from provider p where p.tenant_id = $1
union
select e.provider from inbound_event e where e.tenant_id = $1
union
select r.provider from route r where r.tenant_id = $1
order by 1;
