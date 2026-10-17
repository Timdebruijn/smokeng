# Security policy

## Reporting a vulnerability

Please report it privately, not in a public issue or pull request:

1. Open the **Security** tab of this repository.
2. Choose **Report a vulnerability**.

That opens a private advisory visible only to you and the maintainer, where a fix
can be prepared before anything is public. Include what you found, how to
reproduce it (the smallest request, file or configuration that shows it), which
version it applies to, and what an attacker gains.

This is a one-person project. Reports are read and answered as soon as possible,
with no promised response time. A fix is released as a new version and the
advisory credits you unless you prefer otherwise.

## Which versions

Fixes are made against the latest release and `main`. Older releases are not
patched; upgrade. The release notes and `docs/operations.md` say what changes on
upgrade.

## What is in scope

smokeng measures networks and serves what it measured, so the things that matter
here are:

- **Authentication and authorisation**: reading or changing anything without the
  right session or grant, or seeing another tenant's targets, paths, baselines or
  agents (see `docs/access-control.md`).
- **The agent protocol**: an enrolled or unenrolled agent writing outside its own
  series, replaying or forging a submission, or exhausting the master's memory.
- **Untrusted input**: the SmokePing and TOML importers, the Arrow measurements
  stream the browser reads, and the HTTP API.
- **Secrets**: the session key in the database, OIDC and webhook credentials,
  irtt HMAC keys, and agent private keys.
- **The released binaries and the release process.**

Out of scope: an attacker who already controls the host or the database file, a
global administrator acting as one, and findings that need a configuration the
documentation tells you not to use (for example running unauthenticated on a
routable address).

## Verifying a release

Release binaries come with `SHA256SUMS`, and from the release that adds them,
a signed build-provenance attestation that ties each binary to the workflow run
that built it:

```
gh attestation verify smokeng-linux-amd64 --repo Timdebruijn/smokeng
```
