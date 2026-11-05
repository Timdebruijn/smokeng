# Authentication

For operators wiring smokeng to an identity provider (IdP): what it must send, and what a
session does and does not guarantee.

smokeng authenticates against any OIDC provider using the authorization-code flow with
PKCE. There is no local user database, by design: account lifecycle, MFA and group
membership are your IdP's job, and duplicating them here would only make them wrong in a
second place.

## Without OIDC

If no issuer is configured, smokeng treats every request as an admin — and refuses to
listen on anything but a loopback address. Binding elsewhere requires
`--i-know-this-is-unauthenticated`, which is spelled that way on purpose.

This is the right configuration for a laptop or a single-user box behind a firewall. It is
not a configuration to expose.

## With OIDC

```bash
smokeng serve \
  --listen 0.0.0.0:8080 \
  --oidc-issuer https://auth.example.org/application/o/smokeng/ \
  --oidc-client-id smokeng \
  --oidc-client-secret-file /var/lib/smokeng/oidc-client-secret \
  --oidc-redirect-url https://smokeng.example.org/auth/callback \
  --oidc-admin-claim groups \
  --oidc-admin-value smokeng-admins
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--oidc-issuer` | — | Issuer URL; setting it enables authentication |
| `--oidc-client-id` | — | Client id registered with the provider |
| `--oidc-client-secret-file` | — | File holding the client secret. **Prefer this.** |
| `--oidc-client-secret` | — | Client secret inline — see the warning below |
| `--oidc-redirect-url` | `{external-url}/auth/callback`, else `http://{listen}/auth/callback` | Must match the provider's registration exactly |
| `--oidc-admin-claim` | `groups` | The ID-token claim listing the user's groups |
| `--oidc-admin-value` | — | Membership in this group grants admin. Required with `--oidc-issuer`, unless you pass `--oidc-everyone-is-admin`. |
| `--oidc-everyone-is-admin` | off | Every user the provider authenticates is an admin, who can edit and delete the tree, enrol agents and write grants. A choice you make, not a default; not with `--oidc-admin-value`. |

## What smokeng needs from the IdP

- **Issuer URL** (`--oidc-issuer`). smokeng runs OIDC discovery against it at startup and
  will not start if discovery fails.
- **Redirect URL**: `{external-url}/auth/callback`, or whatever you pass as
  `--oidc-redirect-url`. Register exactly that.
- **Scopes requested**: `openid`, `profile`, `email`, and a scope named after the groups
  claim (`groups` by default; it follows `--oidc-admin-claim`).
- **A groups claim in the ID token**, named by `--oidc-admin-claim`. smokeng reads the ID
  token only, not the userinfo endpoint. The claim may be an array of strings or one
  comma-separated string. A space is part of a group name, so `Domain Admins` is one group.
- **PKCE** (S256) and a client secret: register a confidential client.

If the claim is missing, nobody is an admin and grants never match. Users are demoted, not
promoted: everyone without the admin group is a viewer.

| IdP | Make the groups claim appear | What the claim contains |
| --- | --- | --- |
| Authentik | Make sure the provider has a scope mapping that emits `groups`, and that it is selected on the provider. | Group names. |
| Keycloak | Client scope with a *Group Membership* mapper, claim name `groups`, added to the client. Turn off *Full group path*. | Names; with full path on, `/parent/name`. |
| Microsoft Entra ID | App registration, *Token configuration*, *Add groups claim*. | Group **object IDs** (GUIDs) by default, so `--oidc-admin-value` is a GUID. |
| Google | Not possible. | Google issues no groups claim. |

Check your IdP's documentation for the exact menu names; they move between versions.

**Google** cannot supply groups with plain OIDC, so neither admin nor grants can work. Your
options are `--oidc-everyone-is-admin` (every user Google lets in is an admin; limit who
can sign in at Google, see its documentation), or a proxy or broker in front that adds a groups claim.

**Entra** only helps if the token is not too large. When a user has many groups Entra
leaves the claim out of the token; check your IdP's documentation on group overage and
consider assigning only the groups smokeng needs to the application.

### Worked example: Authentik

1. Create an *OAuth2/OpenID Provider*: client type *Confidential*, redirect URI
   `https://smokeng.example.org/auth/callback` (strict match), scopes `openid`, `profile`,
   `email` and the one that emits `groups`.
2. Create an *Application* with slug `smokeng` and attach the provider. The issuer is then
   `https://auth.example.org/application/o/smokeng/`.
3. Create a group `smokeng-admins` and add yourself. Use *Policy bindings* on the
   application to limit who may sign in at all.
4. Copy the client id and secret into smokeng (secret as a file, see below) and use the
   command at the top of this section.

Verify before relying on it: sign in and try an admin-only action. If you are refused, the
claim is not in the ID token; check the provider's scope mappings.

### Keep the secret off the command line

A process's command line is world-readable in `/proc`, so `--oidc-client-secret` hands
the secret to every local user, to anything that logs `ps` output, and to anyone who can
read the systemd unit that carries it — units are mode 0644 by convention. Put it in a
file only the service user can read:

```bash
install -o smokeng -g smokeng -m 600 /dev/null /var/lib/smokeng/oidc-client-secret
printf %s "$SECRET" > /var/lib/smokeng/oidc-client-secret
```

smokeng trims surrounding whitespace, refuses an empty file, and logs a warning if the
file is readable by anyone but its owner. The two flags are mutually exclusive: passing
both is an error rather than a silent precedence rule, so there is never a question about
which secret is in use.

The Ansible role does this for you — set `smokeng_oidc_client_secret` from a vault and it
is written 0600 and passed as a file.

If a secret has ever been on a command line, treat it as disclosed and rotate it at the
provider. Moving it to a file afterwards does not un-publish it.

## Roles

`admin` comes from the ID token: if the claim named by `--oidc-admin-claim` contains
`--oidc-admin-value`, the user is a global admin. smokeng will not start with
`--oidc-issuer` and no `--oidc-admin-value` unless you pass `--oidc-everyone-is-admin`:
without a group, **every authenticated user is an admin**, which is fine for a small team
that controls who the provider lets through and wrong for anyone else.

Everyone else gets what their **grants** give them, plus whatever `--default-role` allows
a user with no grants. That is where per-subtree access lives — one customer seeing only
their own targets, for instance — and it has its own guide:
[Access control](access-control.md).

Restrict *who may authenticate at all* at the provider. smokeng decides what a signed-in
user may reach; it does not decide who may sign in.

## Sessions

After a successful callback, smokeng sets a `smokeng_session` cookie: HMAC-signed,
`HttpOnly`, `SameSite=Lax`, and `Secure` unless you are on plain HTTP (see
[Behind a proxy](operations.md#behind-a-proxy)). It holds the subject, email, display name,
role, groups and expiry. It holds no provider tokens, and there is no server-side session
store.

That has consequences for your threat model:

- **Logout does not revoke.** `POST /auth/logout` clears the cookie in that browser. A copy
  of the cookie stays valid until it expires, 12 hours after login.
- **Group membership is fixed at login.** Removing someone from a group at the IdP, or
  disabling their account, takes effect when their session expires, not before.
- **Rotating `session_key` logs everyone out**, and is the only way to cut sessions short.
  The recipe is under [Backups](operations.md#backups).

## Prometheus scraping

`/metrics` sits behind an **admin** session, which a scraper cannot provide. Pass
`--metrics-public` to serve it without one, and restrict access at the reverse proxy or by
putting the scraped instance on a management network.
