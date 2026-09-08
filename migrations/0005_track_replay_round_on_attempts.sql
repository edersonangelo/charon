-- Replay resets the attempt counter, so without the round an operator cannot
-- tell the first run's attempt 3 from the second run's attempt 3.
alter table delivery_attempt add column round integer not null default 0;

create index delivery_attempt_round_idx
    on delivery_attempt (delivery_id, round desc, attempted_at desc);
