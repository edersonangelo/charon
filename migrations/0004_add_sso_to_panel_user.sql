alter table panel_user alter column password_hash drop not null;

alter table panel_user add column oidc_subject text unique;

alter table panel_user add constraint panel_user_can_sign_in
    check (password_hash is not null or oidc_subject is not null);
