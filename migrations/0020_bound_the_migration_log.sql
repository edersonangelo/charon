-- The migrator makes its own table before any migration runs, so a database
-- that started before this carries the column it made then. Stated here as
-- well so the schema reads whole, whichever end it is read from.
create table if not exists schema_migration (
    name       varchar(120) primary key,
    applied_at timestamptz  not null default now()
);

alter table schema_migration alter column name type varchar(120);
