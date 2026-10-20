-- A route delivers in order only while it catches up on what was waiting when
-- somebody gave it a past to deliver: a new route told to deliver what was
-- already recorded, a destination switched back on, an address corrected. What
-- arrives meanwhile waits behind it, so the destination still sees everything
-- in order. Once nothing from before this instant waits, the column is cleared
-- and the route delivers side by side again, where one waiting out a retry
-- holds nobody.
alter table route add column catching_up_to timestamptz;

-- A route already working through what was waiting goes on doing it in order:
-- switching it to side by side halfway would send the rest out of order. What
-- it is working through includes what planning has not reached yet, read the
-- way MarkRoutesCaughtUp reads it. Each tenant is visited as itself, so the
-- policies decide what this reads.
do $$
declare
    each_tenant uuid;
begin
    for each_tenant in select id from tenant loop
        perform set_config('charon.tenant', each_tenant::text, true);
        update route r
        set catching_up_to = date_trunc('milliseconds', now()) + interval '1 millisecond'
        where r.tenant_id = each_tenant
          and (exists (
                   select 1 from delivery dl
                   where dl.tenant_id = each_tenant and dl.state = 'pending'
                     and dl.destination_id = r.destination_id and dl.event_provider = r.provider
               )
               or exists (
                   select 1 from inbound_event unplanned
                   where unplanned.tenant_id = each_tenant
                     and unplanned.provider = r.provider
                     and unplanned.planned_at is null
                     and unplanned.id >= first_id_recorded_at(r.events_recorded_from)
                     and (unplanned.signature in ('unchecked', 'valid') or unplanned.override_id is not null)
               ));
    end loop;
    perform set_config('charon.tenant', '', true);
end;
$$;

-- Every claim asks which routes have caught up, and almost always none is
-- catching up.
create index route_catching_up_idx
    on route (tenant_id)
    where catching_up_to is not null;

create index delivery_due_on_route_idx
    on delivery (tenant_id, destination_id, event_provider, next_attempt_at)
    where state = 'pending';

drop function first_waiting_of_each_route();

-- The deliveries that may go, route by route. A route catching up offers only
-- its first waiting delivery, in the order its events were recorded, and only
-- with nothing in flight: one in flight holds the route even when a replay has
-- put an older one ahead of it. That one is offered whether or not it is due,
-- so that the caller decides: claiming wants it only when due, waking wants to
-- know when it will be. Any other route offers whatever is due by due_by,
-- earliest first, up to per_route of them.
--
-- The routes are found by stepping through the index one route at a time, and
-- each reads at most per_route of its own, so the work is bounded by the routes
-- with something waiting times per_route, however long a backlog one of them
-- is catching up on. Reading the due deliveries of every route in one pass
-- instead would read through every backlog being caught up on, on every claim.
--
-- A route catching up is held while an earlier event it wants is not planned
-- yet, because a dispatcher running beside this one may be about to plan it.
-- One whose signature was refused will never be delivered and holds nothing. A
-- dead delivery is no longer waiting, so it lets the next one go.
--
-- A delivery whose route was removed is no longer catching up on anything.
--
-- It is plpgsql so that it is planned on its own: written in SQL it was folded
-- into whichever query called it, and planned there as a scan of every waiting
-- delivery.
create function deliveries_free_to_go(due_by timestamptz, per_route int) returns setof uuid
    language plpgsql stable rows 10
as $$
begin
    return query
    with recursive waiting (destination_id, event_provider) as (
        (select dl.destination_id, dl.event_provider
         from delivery dl
         where dl.tenant_id = current_tenant() and dl.state = 'pending'
         order by dl.destination_id, dl.event_provider
         limit 1)
        union all
        (select later.destination_id, later.event_provider
         from waiting w
         cross join lateral (
             select dl.destination_id, dl.event_provider
             from delivery dl
             where dl.tenant_id = current_tenant() and dl.state = 'pending'
               and (dl.destination_id, dl.event_provider) > (w.destination_id, w.event_provider)
             order by dl.destination_id, dl.event_provider
             limit 1
         ) later)
    )
    select free.id
    from waiting w
    left join route r
        on r.tenant_id = current_tenant()
       and r.destination_id = w.destination_id and r.provider = w.event_provider
    cross join lateral (
        (select head.id
         from (
             select dl.id, dl.event_id
             from delivery dl
             where dl.tenant_id = current_tenant() and dl.state = 'pending'
               and dl.destination_id = w.destination_id and dl.event_provider = w.event_provider
             order by dl.event_id
             limit 1
         ) head
         where r.catching_up_to is not null
           and not exists (
               select 1 from delivery flying
               where flying.tenant_id = current_tenant() and flying.state = 'pending'
                 and flying.destination_id = w.destination_id and flying.event_provider = w.event_provider
                 and flying.leased_until is not null and flying.leased_until > now()
           )
           -- Asked per route rather than joined, so it reads only the range
           -- between where the route starts and its first waiting delivery.
           -- What counts as wanted is what MarkRoutesCaughtUp counts.
           and not (select exists (
               select 1
               from inbound_event unplanned
               where unplanned.tenant_id = current_tenant()
                 and unplanned.provider = w.event_provider
                 and unplanned.planned_at is null
                 and unplanned.id >= first_id_recorded_at(r.events_recorded_from)
                 and unplanned.id < head.event_id
                 and (unplanned.signature in ('unchecked', 'valid') or unplanned.override_id is not null)
               order by unplanned.tenant_id, unplanned.provider, unplanned.id
           )))
        union all
        (select dl.id
         from delivery dl
         where r.catching_up_to is null
           and dl.tenant_id = current_tenant() and dl.state = 'pending'
           and dl.destination_id = w.destination_id and dl.event_provider = w.event_provider
           and dl.next_attempt_at <= due_by
           and (dl.leased_until is null or dl.leased_until <= now())
         order by dl.next_attempt_at
         limit per_route)
    ) free;
end;
$$;
