# Single sign-on

Charon accepts any OpenID Connect provider, discovered from its issuer url.
Nothing about it is gated: no licence key, no seat count, no call to any
service this project controls.

This page is what a provider has to be told, what Charon does with what it
sends, and how to read the log when it does not do what you expected.

## What Charon needs

Two things, and only the first is standard.

**Who the person is.** The `sub` and `email` claims, which every provider
sends. This is what an account is linked to, so somebody keeps one account even
if their address changes at the provider.

**Where they belong.** A claim carrying a list of values — groups, roles, or
whatever the provider calls them. This is *not* part of OpenID Connect: the
specification standardises identity, not authorisation, so every provider has
its own switch for it and none of them turns it on by default. Configuring that
switch is the one step you cannot skip.

## Where a value puts somebody

A value is read as a path.

| the provider sends | where it puts them |
|---|---|
| `/acme/operator` | tenant `acme`, as the role `operator` |
| `/acme` | tenant `acme`, and the role is decided in the panel |
| `acme` | the same: a leading separator carries no meaning |
| `/acme/wizard` | tenant `acme`, at the least — no role goes by that name, and the log says so |
| `finance-team` | nothing, unless a tenant is named that, or the value is pointed at one |

Three rules follow from that:

- **The name is the mapping.** A deployment whose groups are named after its
  tenants registers nothing at all. An empty configuration is a working one.
- **A role is only taken from the provider when the value names one.** Without
  the second part of the path, who holds what inside a tenant is decided in the
  panel and survives every sign-in.
- **Belonging without a role stated is belonging as a viewer**, which is the
  least Charon ships.

### When the names do not match

A directory that names its groups its own way — `SEC-CHARON-PROD-ADMINS` — is
not going to be renamed to suit Charon. Point the value at a tenant instead, in
**settings → roles**, under *Values that arrive from single sign-on*: the value,
the role it arrives as, and the way of signing in it comes from.

A value that is pointed somewhere goes there whatever its name would have said.
Everything else falls back to the name.

## The token is what says where somebody belongs

Every sign-in settles it: the tenants the values name are the tenants the person
belongs to, and no others.

Take somebody out of a group at the provider and their next sign-in takes the
tenant with it. Add them to one and they have it without anybody here doing
anything. An identity that names no tenant at all is refused, and no account is
made from it.

What the token says nothing about — the role held inside a tenant, when the
value did not name one — is left exactly as the panel set it.

## Keycloak

The client, once:

**Clients → Create client**

| | |
|---|---|
| Client ID | `charon` |
| Client authentication | On |
| Valid redirect URIs | `https://charon.example.com/auth/oidc/callback` |

Copy the secret from the **Credentials** tab.

Then the part that is easy to miss: **Keycloak does not put group membership in
any token by default.** Adding somebody to a group changes nothing that Charon
can see until this mapper exists.

**Clients → charon → Client scopes → charon-dedicated → Add mapper → By
configuration → Group Membership**

| | |
|---|---|
| Name | `groups` |
| Token Claim Name | `groups` |
| Full group path | **On** |
| Add to ID token | On |
| Add to access token | On |
| Add to userinfo | On |

**Full group path** is what makes a nested group arrive as `/acme/operator`
instead of just `operator`. Off, Charon sees the leaf and has no idea which
tenant it belongs to.

Now the groups, in **Groups**, one child per role:

```
acme
├── owner
├── admin
├── operator
└── viewer
globex
└── operator
```

Somebody in `/acme/owner` and `/globex/operator` is an owner in one tenant and
an operator in the other, and moves between them with the selector in the
panel's header.

Before signing in, check what the token will carry: **Clients → charon → Client
scopes → Evaluate**, choose a user, then **Generated ID token**. The `groups`
claim shows there before anybody tries to log in.

## Other providers

The mechanism is the same everywhere; only the switch differs.

| provider | where the values come from |
|---|---|
| **Keycloak** | a Group Membership mapper, as above |
| **Okta** | a `groups` claim added to the authorization server, filtered to what the app should see |
| **Entra ID** | the `groups` claim enabled in the app registration. It sends object ids, not names, so point those values at tenants rather than relying on the name |
| **Auth0** | an action that adds a namespaced claim, such as `https://charon.example.com/groups`; name it with `-oidc-tenant-claim` |
| **Google Workspace** | sends no group in any token. Groups have to come from the Admin SDK, which Charon does not read, so place people from the panel |

A claim that is nested is named by its path: `-oidc-tenant-claim
realm_access.roles` reads Keycloak's realm roles instead of its groups, if that
suits a deployment better.

## Configuration

| flag | environment | default | purpose |
|---|---|---|---|
| `-oidc-issuer` | `CHARON_OIDC_ISSUER` | — | issuer url; discovery does the rest |
| `-oidc-client-id` | `CHARON_OIDC_CLIENT_ID` | — | client registered at the provider |
| `-oidc-client-secret` | `CHARON_OIDC_CLIENT_SECRET` | — | secret, when the client is confidential |
| `-oidc-redirect-url` | `CHARON_OIDC_REDIRECT_URL` | — | must end in `/auth/oidc/callback` |
| `-oidc-scopes` | `CHARON_OIDC_SCOPES` | `openid,profile,email` | scopes to request |
| `-oidc-tenant-claim` | `CHARON_OIDC_TENANT_CLAIM` | `groups` | claim whose values place somebody; a path reads a nested claim |
| `-auto-provision` | `CHARON_AUTO_PROVISION` | off | make an account on first arrival, instead of requiring one to exist |
| `-required-claim` | `CHARON_REQUIRED_CLAIM` | — | admit only identities carrying this value |
| `-accept-unverified-email` | `CHARON_ACCEPT_UNVERIFIED_EMAIL` | off | admit identities whose provider does not verify addresses |

Secrets belong in a `.env` file beside `docker-compose.yml`, which is not
committed, or in whatever your deployment uses for them. Nothing in this
repository holds one.

### Addresses that are not verified

With `-accept-unverified-email` off, an identity whose provider says the address
was never verified is refused. That matters where anybody can sign up at the
provider and assert an address; it does not where an administrator creates the
accounts, which is the usual arrangement for an internal directory. Keycloak
leaves `email_verified` false on accounts an administrator made, so an internal
realm normally wants this on.

### Accounts that already exist

With auto-provisioning off — the default — somebody who exists at the provider
still cannot reach the panel until an account with that address exists here.
Signing in then links the account to the provider's subject, and from that point
the link holds even if the address changes.

## The system administrator is not taken from the token

A system administrator belongs to no tenant and reaches every one, and is the
only account that creates and removes tenants. That is deliberately the one
thing a provider cannot grant: everything else it says is scoped to a tenant,
and this is not.

It is granted by somebody who already is one, in **settings → operators**, or
on the command line for the first:

```sh
charon user superadmin -email you@example.com
```

## When it does not work

If the claim you configured carried nothing, one line says which claims did
arrive and which of them hold a list of names, so a claim in the wrong place is
found without guessing:

```
the identity carried nothing under the claim asked for
  claim=groups
  looked_in="id token, userinfo, access token"
  claims_present=[acr, aud, email, …]
  claims_that_carry_a_list_of_names=[realm_access.roles, resource_access.charon.roles]
```

Charon looks in all three of those places before giving up, so a provider that
puts its groups in only one of them still works.

At `debug`, every arrival is logged with the values it carried and whether the
address came verified. Tokens themselves are never logged, decoded or raw: one
in a log file is a usable credential.

Common causes, in the order they happen:

| the log says | what to change |
|---|---|
| `carried nothing under the claim asked for` | the mapper does not exist at the provider, or it does not add to any token |
| values arrive as leaf names, not paths | **Full group path** is off in Keycloak |
| `nothing this identity carries names a tenant` | no tenant is named after the value, and the value is not pointed at one |
| `the address is not verified at the provider` | turn on `-accept-unverified-email`, or verify the address at the provider |
