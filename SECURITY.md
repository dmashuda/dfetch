# Security Policy

## Supported versions

dfetch is released from `main`. Security fixes are applied to `main` and shipped
in the next tagged release; only the latest release is supported.

## Reporting a vulnerability

Please report security vulnerabilities **privately** — do not open a public
issue, PR, or discussion for a suspected vulnerability.

Use GitHub's private vulnerability reporting:

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Describe the issue, affected version/commit, and reproduction steps.

You can expect an initial response within a few days. Once a fix is available it
will be released and the advisory published, crediting the reporter unless you
prefer to remain anonymous.

## Scope

dfetch runs SQL locally against a per-request SQLite database and fetches data
from the data sources you configure. Most relevant to security:

- Credentials are read from the environment (e.g. `GITHUB_TOKEN`, `JAEGER_TOKEN`,
  `CKAN_API_KEY`), never from config files.
- Connectors make outbound requests to the hosts you point them at; review your
  `dfetch.yaml` `base_url`s and saved queries before running untrusted configs.
