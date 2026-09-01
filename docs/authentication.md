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
| `--oidc-admin-value` | — | Membership in this group grants admin. **Empty means every authenticated user is an admin**, and the API can edit and delete the tree. |

Register `https://your-host/auth/callback` as the redirect URI with your provider, and
make sure the claim named by `--oidc-admin-claim` is actually included in the ID token —
in Authentik and Keycloak that is a scope or mapper you have to add explicitly.

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
