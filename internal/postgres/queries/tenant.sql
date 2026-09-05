-- name: CreateTenant :one
insert into tenant (slug, name) values ($1, $2) returning *;

-- name: TenantBySlug :one
select * from tenant where slug = $1;

-- name: Tenants :many
select * from tenant order by slug;

-- name: DeleteTenant :exec
delete from tenant where slug = $1;
