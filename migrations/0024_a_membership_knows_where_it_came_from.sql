-- Where a membership came from decides who may take it away. One the identity
-- provider granted is the provider's to withdraw, and is settled again at
-- every sign-in; one given here answers to nobody but whoever gave it.
create table membership_source (
    name        varchar(20)  primary key,
    description varchar(200) not null
);

insert into membership_source (name, description) values
    ('granted',  'somebody here put them in this tenant'),
    ('provider', 'a value the identity provider sends names this tenant');

alter table membership add column source varchar(20) not null default 'granted'
    references membership_source (name);

alter table membership alter column source drop default;
