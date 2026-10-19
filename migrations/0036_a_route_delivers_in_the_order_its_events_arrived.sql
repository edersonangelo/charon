create index delivery_waiting_by_destination_idx
    on delivery (destination_id)
    where state = 'pending';
