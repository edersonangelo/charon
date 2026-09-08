-- name: SetRetention :execrows
update tenant set retention_days = $2 where slug = $1;

-- name: Retentions :many
select id, slug, retention_days from tenant order by slug;

-- An event is only discarded once nothing is still trying to deliver it. A
-- pending delivery is live work, and its age says the destination has been
-- unreachable for a long time, which is a reason to look rather than to erase.
-- name: PurgeEvents :execrows
delete from inbound_event e
where e.tenant_id = $1
  and e.received_at < now() - make_interval(days => sqlc.arg(days)::int)
  and not exists (
      select 1 from delivery d
      where d.event_id = e.id and d.state = 'pending'
  );

-- name: PurgeableEvents :one
select count(*) from inbound_event e
where e.tenant_id = $1
  and e.received_at < now() - make_interval(days => sqlc.arg(days)::int)
  and not exists (
      select 1 from delivery d
      where d.event_id = e.id and d.state = 'pending'
  );
