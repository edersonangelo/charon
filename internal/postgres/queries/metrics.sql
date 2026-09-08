-- Every number a scrape reports, one tenant at a time. A scrape covers the
-- whole deployment, but each query still names the tenant it counts: the
-- tenant is pinned on the connection, not on the statement, so a query that
-- left it out would count whatever the pooled connection happened to be
-- pinned to and report it under every tenant's name.

-- name: EventTotals :many
select t.slug, e.signature, count(*) as total
from inbound_event e join tenant t on t.id = e.tenant_id
where e.tenant_id = $1
group by t.slug, e.signature;

-- name: DeliveryTotals :many
select t.slug, d.state, count(*) as total
from delivery d join tenant t on t.id = d.tenant_id
where d.tenant_id = $1
group by t.slug, d.state;

-- name: AttemptTotals :many
select t.slug, count(*) as total
from delivery_attempt a join tenant t on t.id = a.tenant_id
where a.tenant_id = $1
group by t.slug;

-- name: OldestPending :many
select t.slug, coalesce(extract(epoch from now() - min(d.next_attempt_at)), 0)::float8 as seconds
from delivery d join tenant t on t.id = d.tenant_id
where d.state = 'pending' and d.tenant_id = $1
group by t.slug;

-- name: CountOperators :one
select count(*) from panel_user;
