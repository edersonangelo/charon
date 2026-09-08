-- Verification stops being a list of vendors and becomes a set of parameters.
-- A vendor is now a named set of these values, not a branch in the code.
alter table provider drop constraint provider_verifier_known;

alter table provider add column scheme        text not null default 'simple';
alter table provider add column algorithm     text not null default 'sha256';
alter table provider add column encoding      text not null default 'hex';
alter table provider add column timestamp_key text not null default 't';
alter table provider add column signature_key text not null default 'v1';

update provider set verifier = 'hmac', scheme = 'simple',
                    signature_header = coalesce(signature_header, 'X-Signature')
where verifier = 'hmac-sha256';

update provider set verifier = 'hmac', scheme = 'simple',
                    signature_header = 'X-Hub-Signature-256'
where verifier = 'github';

update provider set verifier = 'hmac', scheme = 'advanced',
                    signature_header = 'Stripe-Signature'
where verifier = 'stripe';

alter table provider
    add constraint provider_verifier_known
        check (verifier in ('hmac', 'shared-token', 'basic-auth')),
    add constraint provider_scheme_known
        check (scheme in ('simple', 'advanced')),
    add constraint provider_algorithm_known
        check (algorithm in ('sha1', 'sha256', 'sha512')),
    add constraint provider_encoding_known
        check (encoding in ('hex', 'base64'));
