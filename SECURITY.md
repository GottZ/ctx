# Security Policy

## Reporting a vulnerability

Please report suspected vulnerabilities **privately**, through GitHub's private
vulnerability reporting: open the [Security tab](https://github.com/GottZ/ctx/security)
and choose **Report a vulnerability**
([direct link](https://github.com/GottZ/ctx/security/advisories/new)).

Please do not open a public issue, pull request or discussion for a suspected
vulnerability. Hardening suggestions without an exploitable impact are welcome as
regular issues.

## Supported versions

Security fixes land on the main line (`root`) and ship in the next release. Only the
**newest minor version** is supported; older minor versions are not patched — the fix
is to upgrade.

| Version                                              | Security fixes |
|------------------------------------------------------|----------------|
| newest minor (e.g. `5.23.x` while 5.23 is current)   | yes            |
| any older minor                                      | no — upgrade   |

## What to include

- the affected version (release tag or commit) and how ctx runs: compose stack,
  container image, bare binary
- the affected component — HTTP API, MCP, OAuth, CLI, web UI, migrations, scripts
- reproduction steps or a proof of concept, with expected and actual behaviour
- the impact as you see it: what an attacker gains, and what they need first
  (network position, an API key and its tier, host access)
- whether and how you would like to be credited

## What happens next

The report is read by the maintainer, reproduced, and answered in the advisory
thread. There is no guaranteed response time; the aim is to acknowledge quickly and
to keep you informed while a fix is under way. A confirmed issue is fixed on the main
line, released, and published as a GitHub Security Advisory with credit to the
reporter unless you prefer otherwise; the disclosure date is coordinated with you.
Published advisories: [github.com/GottZ/ctx/security/advisories](https://github.com/GottZ/ctx/security/advisories).
