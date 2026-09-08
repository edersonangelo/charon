-- Identifiers are the database's to make. PostgreSQL 18 generates them ordered
-- by time, which is what the application was doing by hand, so nothing has to
-- know an identifier before the row it belongs to exists.
alter table tenant           alter column id set default uuidv7();
alter table inbound_event    alter column id set default uuidv7();
alter table destination      alter column id set default uuidv7();
alter table route            alter column id set default uuidv7();
alter table delivery         alter column id set default uuidv7();
alter table delivery_attempt alter column id set default uuidv7();
alter table panel_user       alter column id set default uuidv7();

-- A tenant is found by its slug and a role by its name, so the identifier of
-- either can move. Everything that points at one follows it, including what
-- points at a role, which carries the tenant in its key. Referential integrity
-- carries the change through row level security, which is why this works
-- whatever role applies the migration.
do $$
declare
    reference record;
begin
    for reference in
        select conrelid::regclass as child, conname as name,
               pg_get_constraintdef(oid) as definition
        from pg_constraint
        where contype = 'f'
          and confrelid in ('tenant'::regclass, 'role'::regclass)
          and pg_get_constraintdef(oid) not like '%ON UPDATE%'
    loop
        execute format('alter table %s drop constraint %I', reference.child, reference.name);
        execute format('alter table %s add constraint %I %s on update cascade',
            reference.child, reference.name, reference.definition);
    end loop;
end
$$;

-- The tenant every deployment has was seeded with a written out identifier so
-- the code could name it. Nothing names it now.
update tenant set id = uuidv7() where id = '00000000-0000-0000-0000-000000000001';
