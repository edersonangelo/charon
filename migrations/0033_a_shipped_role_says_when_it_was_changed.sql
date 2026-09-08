-- Starting records the roles Charon ships, and left alone any that already
-- granted something, so that a deployment's own decision would survive a
-- restart. That rule cannot tell a role somebody narrowed on purpose from one
-- that has simply been there since before a permission existed: both grant
-- something, so both were skipped, and a permission added by a new version
-- reached nobody who was already running.
--
-- Saying it outright instead of guessing from whether anything is granted. A
-- role is changed when somebody changes it, and only then.
alter table role add column changed boolean not null default false;

-- Nothing is known about what came before, and treating everything as changed
-- would leave every existing deployment behind for good. Treating it as
-- untouched puts the shipped roles back in step with the build once, which is
-- the point; a deployment that had narrowed one narrows it again and it sticks.
