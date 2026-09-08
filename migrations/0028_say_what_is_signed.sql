-- Whether a secret can actually be read is known only by the process that
-- delivers, and the panel runs somewhere else with a different environment. It
-- cannot look, so it is told: the dispatcher writes what it found, and the
-- panel reports that rather than guessing from its own surroundings.
create table signing_secret_check (
    signing_secret_id uuid         primary key references signing_secret (id) on delete cascade,
    tenant_id         uuid         not null references tenant (id) on delete cascade,
    readable          boolean      not null,
    detail            varchar(200) not null default '',
    checked_at        timestamptz  not null default now()
);

alter table signing_secret_check enable row level security;
alter table signing_secret_check force row level security;

create policy tenant_isolation on signing_secret_check
    using (tenant_id = current_tenant())
    with check (tenant_id = current_tenant());

-- What an attempt was signed with, as it was at that moment. An attempt is a
-- log entry and never changes, so holding the references it used is a record
-- of what happened rather than a second copy of what is configured now: after
-- a rotation the configured set no longer says which one went out when.
alter table delivery_attempt
    add column signed_with varchar(400) not null default '';
