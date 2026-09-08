-- A signature that failed is never delivered, and that is right almost always.
-- Almost: a proxy that strips a header, a sender that rotated a secret without
-- saying so, a provider that changed its format. The request is genuine, it is
-- recorded whole, and there is no way to have it sent again from the far side.
--
-- So the refusal can be overruled, deliberately, by somebody who says why. The
-- reason is a column and not a comment because it is the only record of a
-- decision that let something past a check.
create table signature_override (
    id         uuid         primary key default uuidv7(),
    tenant_id  uuid         not null references tenant (id) on delete cascade,
    reason     varchar(500) not null,
    decided_by uuid         not null references panel_user (id),
    decided_at timestamptz  not null default now(),
    -- Long enough to be an explanation. "ok" is not one.
    constraint signature_override_has_a_reason check (length(btrim(reason)) >= 10)
);

create index signature_override_tenant_idx on signature_override (tenant_id);

alter table signature_override enable row level security;
alter table signature_override force row level security;

create policy tenant_isolation on signature_override
    using (tenant_id = current_tenant())
    with check (tenant_id = current_tenant());

-- What the signature said and what an operator decided are two different
-- things, and both stay readable. The verdict is never rewritten: an event
-- delivered this way is still invalid or missing, and now also says who let it
-- through and why.
alter table inbound_event
    add column override_id uuid references signature_override (id);

create index inbound_event_override_idx on inbound_event (override_id)
    where override_id is not null;

-- Recorded on the attempt too, so the page that shows what happened does not
-- have to reach for the event to explain why something that failed went out.
alter table delivery_attempt
    add column forced boolean not null default false;
