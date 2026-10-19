-- A route made before anybody was asked delivered everything ever recorded,
-- and keeps doing so.
alter table route
    add column events_recorded_from timestamptz not null default '-infinity';

-- When an event was recorded is read off its identifier, a uuidv7 the
-- database makes, rather than off the instant the request carries: that comes
-- from the clock of whichever host took it in, and a host running fast or slow
-- would decide what a route skips. The lowest identifier of an instant turns
-- "recorded since" into a range an index can answer, so what a route skipped
-- is never read again.
create function first_id_recorded_at(a_at timestamptz) returns uuid
    language sql immutable
as $$
    select case
        when a_at = '-infinity' then '00000000-0000-0000-0000-000000000000'::uuid
        else (lpad(to_hex((extract(epoch from a_at) * 1000)::bigint), 12, '0')
              || '00000000000000000000')::uuid
    end;
$$;

create index inbound_event_unplanned_by_provider_idx
    on inbound_event (tenant_id, provider, id)
    where planned_at is null;

-- A refused signature is settled as soon as it is planned, whether or not any
-- route wants it, so planning finds those on their own.
create index inbound_event_refused_unsettled_idx
    on inbound_event (tenant_id, id)
    where planned_at is null and signature in ('invalid', 'missing') and override_id is null;
