-- How a provider's signature is checked. The secret itself is never stored
-- here: the row names the environment variable that carries it, so a database
-- dump cannot be used to forge a signature.
create table provider (
    name              text        primary key,
    verifier          text        not null,
    secret_env        text        not null,
    signature_header  text,
    tolerance_seconds integer     not null default 300,
    created_at        timestamptz not null default now(),
    constraint provider_verifier_known
        check (verifier in ('hmac-sha256', 'stripe', 'github'))
);

-- unchecked: no verifier configured for the provider
-- valid:     the signature matched
-- invalid:   a signature was present and did not match
-- missing:   a verifier is configured and the request carried no signature
alter table inbound_event
    add column signature text not null default 'unchecked'
    constraint inbound_event_signature_known
        check (signature in ('unchecked', 'valid', 'invalid', 'missing'));

create index inbound_event_signature_idx
    on inbound_event (signature, received_at desc)
    where signature <> 'valid';
