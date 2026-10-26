# Security policy

## Supported versions

Cloud is in alpha. Security fixes go into the latest release (and `main`)
only; please upgrade to it before reporting.

| Version | Supported |
|---|---|
| Latest release | Yes |
| Older releases | No |

## Reporting a vulnerability

**Please do not open a public issue, discussion or pull request for a
vulnerability.**

Report it privately through GitHub Security Advisories: open the
[Security tab](https://github.com/do-service/cloud/security) of the
repository and choose **Report a vulnerability**
([direct link](https://github.com/do-service/cloud/security/advisories/new)).

Helpful details:

- the affected version or commit and how Cloud is deployed (compose, reverse
  proxy, enabled editors and OIDC providers);
- steps to reproduce or a proof of concept;
- the impact as you see it (data exposure, privilege escalation, denial of
  service, ...).

We aim to acknowledge a report within a few working days, keep you updated
in the advisory, and credit you in it once a fix is released, unless you
prefer to stay anonymous.

## Scope

In scope: the server (`backend/`), the web UI (`frontend/`), the desktop
client (`desktop/`), the `Dockerfile` and the default `docker-compose.yml`.
Vulnerabilities in OnlyOffice, Collabora, Keycloak or PostgreSQL themselves
belong to those projects; an insecure way our compose file configures them is
in scope.
