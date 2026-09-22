-- Every instant here is recorded as timestamptz and stays that way: the
-- database holds the moment, not somebody's reading of it. What differs is who
-- is looking, so each person carries the zone their panel renders in, named
-- the way the tz database names it. Empty is UTC, which is what the panel
-- showed everybody before this column existed.
alter table panel_user add column time_zone varchar(64) not null default '';
