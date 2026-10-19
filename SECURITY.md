# Security policy

## Supported versions

`0.1.x` receives security fixes.

## Reporting a vulnerability

Do not open a public issue. Use GitHub's private vulnerability reporting:

https://github.com/xiaolai/amgo/security/advisories/new

Include the amgo version, platform, a minimal reproduction, and the impact.
Do not include live tokens, credentials, private message bodies, or private
network details. If a credential may have leaked, rotate it first.

## Scope

In scope:

- the `amgo` binary, adapters, state handling and release artifacts;
- cases where amgo claims delivery or completion that the evidence does not
  support;
- path, socket, key-file or permission checks that fail open.

Out of scope:

- a hostile process running as the same OS user. That user is the trust
  boundary; amgo is not a sandbox or an authorization system.
- vulnerabilities in Claude Code, Codex or the providers themselves. Report
  those to the provider.
- a native hold, refusal or rate-limit drop that amgo honestly reports as
  `submitted` with `native_disposition=unknown`.

## Disclosure

Reports are acknowledged on a best-effort basis. Please allow time for a fix
and a coordinated disclosure before publishing details.
