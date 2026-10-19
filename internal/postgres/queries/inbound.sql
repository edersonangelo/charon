-- An event's identifier is made when it is inserted and becomes visible when
-- its transaction commits. Two of the same provider recorded side by side could
-- commit in the other order, and the later identifier be seen, planned and
-- delivered before the earlier one existed. Recording one provider's events one
-- at a time keeps the order they become visible in the order of their
-- identifiers, which is the order they are delivered in.
-- name: HoldProviderForRecording :exec
select pg_advisory_xact_lock(1, hashtext(sqlc.arg(tenant_id)::uuid::text || '/' || sqlc.arg(provider)::text));

-- name: CreateInboundEvent :one
insert into inbound_event (
    tenant_id, provider, path, received_at, body_size, signature
) values (
    $1, $2, $3, $4, $5, $6
)
returning id;

-- name: CreateInboundRequest :exec
insert into inbound_request (
    event_id, tenant_id, headers, body
) values (
    $1, $2, $3, $4
);

-- name: GetInboundEvent :one
select * from inbound_event where id = $1 and tenant_id = $2;

-- name: GetInboundRequest :one
select * from inbound_request where event_id = $1 and tenant_id = $2;

-- name: CountInboundEvents :one
select count(*) from inbound_event;

-- name: ListRecentInboundEvents :many
select * from inbound_event order by received_at desc limit $1;
