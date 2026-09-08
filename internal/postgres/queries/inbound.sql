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
