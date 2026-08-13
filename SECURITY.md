# Security policy

`subgit` accepts OAuth-authorized writes to public GitHub repositories. Treat an instance as a credential-handling service.

## Deployment requirements

- Serve only over HTTPS.
- Keep `GITHUB_OAUTH_CLIENT_SECRET` in a secret store or deployment environment; never commit it.
- Use a dedicated GitHub OAuth App and restrict its callback URL to the exact public service URL.
- Persist `/data` with access limited to the service account. It contains cached Git data.
- Do not log HTTP Authorization headers, OAuth responses, or generated push URLs.

## Reporting vulnerabilities

Do not open a public issue for credential exposure or authorization bypasses. Contact the repository owner privately with reproduction steps and impact.
