-- Two things a column should say and was not saying: how long a value may be,
-- and which values exist at all. A set the code closes belongs in a table with
-- a key pointing at it, the way permissions already are; a length that is a
-- rule belongs in the column, not in whoever remembers to check it.

create table delivery_state (
    name        varchar(20) primary key,
    description varchar(200) not null
);

insert into delivery_state (name, description) values
    ('pending',   'waiting for its next attempt'),
    ('delivered', 'the destination answered 2xx'),
    ('dead',      'the attempt limit was reached and it is not tried again');

create table signature_state (
    name        varchar(20) primary key,
    description varchar(200) not null
);

insert into signature_state (name, description) values
    ('unchecked', 'no verifier is configured for the provider'),
    ('valid',     'the signature matched the configured secret'),
    ('invalid',   'a signature arrived and did not match'),
    ('missing',   'a verifier is configured and the request carried no signature');

-- These three are open to a deployment that adds Go: the rows shipped are the
-- ones Charon knows, and a process registers its own on start, so the database
-- can hold the key without the set being frozen at migration time.
create table transport (
    name        varchar(40) primary key,
    description varchar(200) not null default ''
);

insert into transport (name, description) values
    ('http', 'posts the recorded body to a url');

create table verifier (
    name        varchar(40) primary key,
    description varchar(200) not null default ''
);

insert into verifier (name, description) values
    ('hmac',        'a digest of the body under a shared secret'),
    ('shared-token','a fixed token carried in a header'),
    ('basic-auth',  'a user and password carried in the authorization header');

create table auth_method (
    name        varchar(40) primary key,
    description varchar(200) not null default ''
);

insert into auth_method (name, description) values
    ('password', 'an address and a password held here'),
    ('oidc',     'an OpenID Connect provider');

alter table delivery
    alter column state type varchar(20),
    alter column last_error type varchar(2000),
    add constraint delivery_state_fkey foreign key (state) references delivery_state (name);

alter table delivery_attempt alter column error type varchar(2000);

alter table inbound_event
    alter column provider type varchar(100),
    alter column path type varchar(2048),
    alter column signature type varchar(20),
    add constraint inbound_event_signature_fkey
        foreign key (signature) references signature_state (name);

alter table destination
    alter column name type varchar(100),
    alter column url type varchar(2048),
    alter column transport type varchar(40),
    add constraint destination_transport_fkey
        foreign key (transport) references transport (name);

alter table route alter column provider type varchar(100);

alter table provider
    alter column name type varchar(100),
    alter column verifier type varchar(40),
    alter column secret_env type varchar(100),
    alter column signature_header type varchar(100),
    alter column scheme type varchar(20),
    alter column algorithm type varchar(20),
    alter column encoding type varchar(20),
    alter column timestamp_key type varchar(20),
    alter column signature_key type varchar(20),
    add constraint provider_verifier_fkey foreign key (verifier) references verifier (name);

alter table panel_user
    alter column email type varchar(320),
    alter column password_hash type varchar(100),
    alter column oidc_subject type varchar(255);

alter table tenant
    alter column slug type varchar(63),
    alter column name type varchar(120);

alter table role
    alter column name type varchar(63),
    alter column description type varchar(200);

alter table permission
    alter column name type varchar(60),
    alter column description type varchar(200);

alter table role_mapping
    alter column method type varchar(40),
    alter column claim type varchar(200),
    add constraint role_mapping_method_fkey foreign key (method) references auth_method (name);

-- The helper took the provider as unbounded text, and it is the same name.
drop function routed(text, uuid);
create function routed(a_provider varchar(100), a_destination uuid) returns boolean
    language sql stable
as $$
    select exists (
        select 1 from route r
        join destination d on d.id = r.destination_id and d.enabled
        where r.provider = a_provider and r.destination_id = a_destination
    );
$$;

-- The set is stated once. A key pointing at the rows that exist says what a
-- list repeated inside a check said, and says it in a place a deployment can
-- add to when it registers a transport or a way of signing in.
alter table delivery      drop constraint delivery_state_known;
alter table inbound_event drop constraint inbound_event_signature_known;
alter table provider      drop constraint provider_verifier_known;
