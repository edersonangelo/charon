-- Whether a destination is still routed for a provider and enabled. The panel
-- asks this in several places, and repeating the join in each query made the
-- counts drift apart.
create function routed(a_provider text, a_destination uuid) returns boolean
    language sql
    stable
as $$
    select exists (
        select 1
        from route r
        join destination d on d.id = r.destination_id and d.enabled
        where r.provider = a_provider and r.destination_id = a_destination
    );
$$;
