-- Isolation is enforced by the database, not by remembering to filter. A query
-- that forgets its tenant returns nothing instead of another tenant's rows.
create function current_tenant() returns uuid
    language sql
    stable
as $$
    select nullif(current_setting('charon.tenant', true), '')::uuid;
$$;

do $$
declare
    scoped text;
begin
    foreach scoped in array array[
        'inbound_event', 'inbound_request', 'delivery', 'delivery_attempt',
        'destination', 'route', 'provider'
    ] loop
        execute format('alter table %I enable row level security', scoped);
        -- FORCE so the owner this application connects as is subject to the
        -- policy as well; without it the whole thing is decoration.
        execute format('alter table %I force row level security', scoped);
        execute format($p$
            create policy tenant_isolation on %I
                using (tenant_id = current_tenant())
                with check (tenant_id = current_tenant())
        $p$, scoped);
    end loop;
end
$$;
