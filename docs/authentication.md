# Authentication

smokeng authenticates against any OIDC provider — Authentik, Keycloak, Entra ID, Google,
Okta — using the authorization-code flow with PKCE. There is no local user database, by
design: account lifecycle, MFA and group membership are your identity provider's job, and
duplicating them here would only make them wrong in a second place.

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
  --oidc-client-secret "$SMOKENG_CLIENT_SECRET" \
  --oidc-redirect-url https://smokeng.example.org/auth/callback \
  --oidc-admin-claim groups \
  --oidc-admin-value smokeng-admins
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--oidc-issuer` | — | Issuer URL; setting it enables authentication |
| `--oidc-client-id` | — | Client id registered with the provider |
| `--oidc-client-secret` | — | Client secret |
| `--oidc-redirect-url` | `http://{listen}/auth/callback` | Must match the provider's registration exactly |
| `--oidc-admin-claim` | `groups` | The ID-token claim listing the user's groups |
| `--oidc-admin-value` | — | Membership in this group grants admin |

Register `https://your-host/auth/callback` as the redirect URI with your provider, and
make sure the claim named by `--oidc-admin-claim` is actually included in the ID token —
in Authentik and Keycloak that is a scope or mapper you have to add explicitly.

## Roles

`admin` comes from the ID token: if the claim named by `--oidc-admin-claim` contains
`--oidc-admin-value`, the user is a global admin. The claim is read as a string, a comma-
or space-separated list, or an array. If `--oidc-admin-value` is empty, **every
authenticated user is an admin** — fine for a small team, wrong for a large one.

Everyone else gets what their **grants** give them, plus whatever `--default-role` allows
a user with no grants. That is where per-subtree access lives — one customer seeing only
their own targets, for instance — and it has its own guide:
[Access control](access-control.md).

Restrict *who may authenticate at all* at the provider. smokeng decides what a signed-in
user may reach; it does not decide who may sign in.

## Sessions

After a successful callback, smokeng sets a `smokeng_session` cookie: HMAC-signed,
`HttpOnly`, `SameSite=Lax`, `Secure` unless you are on plain HTTP, valid for 12 hours. It
holds the subject, email, display name, role and expiry — no provider tokens, and no
server-side session store to keep or to lose.

`POST /auth/logout` clears it.

## Prometheus scraping

`/metrics` sits behind the same session as everything else, which a scraper cannot
provide. Pass `--metrics-public` to expose it without one, and restrict access at the
reverse proxy or by binding the metrics-scraped instance to a management network.
