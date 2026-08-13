# subgit

`subgit` exposes a directory inside a public GitHub repository as a normal Git repository. It materializes filtered Git history, so ordinary Git clients see the selected directory as their checkout root. The URL contains one identifier: `OWNER/REPOSITORY/FOLDER`.

## Hosted service

The reference deployment is available at **https://subgit.gakoy.com**. Clone a directory directly by replacing `OWNER/REPOSITORY/FOLDER`:

```sh
git clone https://subgit.gakoy.com/labri-progress/what-are-they-doing/paper.git
git clone https://subgit.gakoy.com/monperrus/test-repo-public/.github.git
```

For an OAuth-authorized write-through push, visit:

```text
https://subgit.gakoy.com/auth/github?return_to=/OWNER/REPOSITORY/FOLDER.git
```

Then copy the push-URL command from the callback page, commit as usual, and run `git push`.

Requested GitHub directories are cached and refreshed periodically. Their virtual history contains commits that affect the selected path, with that path removed from the checkout root.

## Local run

```sh
mkdir -p data
cp config.example.json data/config.json
SUBGIT_CONFIG=$PWD/data/config.json SUBGIT_DATA_DIR=$PWD/data go run .
git clone http://localhost:8080/monperrus/test-repo-public/.github.git
```

`GET /status` reports recent materializations.

## GitHub OAuth App setup

Register a GitHub OAuth App with callback URL `https://HOST/auth/github/callback`, then provide `GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET`, and `SUBGIT_PUBLIC_URL=https://HOST` as service environment variables. The OAuth App must request `repo workflow`; the `workflow` permission is required when a selected directory contains `.github/workflows/`.

Users begin authorization at:

```text
https://HOST/auth/github?return_to=/OWNER/REPOSITORY/FOLDER.git
```

The callback creates an eight-hour opaque Git HTTPS password and displays a command that sets the remote's push URL. It is not a GitHub token. A push updates the virtual repository, then subgit projects its tree into the selected folder in the upstream repository and pushes that commit to GitHub with the OAuth access token. The token stays in process memory and expires with the push session.

```sh
git clone https://HOST/OWNER/REPOSITORY/FOLDER.git
cd FOLDER
# complete the browser authorization above and set the callback's push URL
git add . && git commit -m "Update selected folder" && git push
```

## Operational limits

- Only public GitHub repositories and their `main` branch are currently supported.
- A virtual push is acknowledged before its asynchronous upstream projection completes. Check service logs/status when operating this in production.
- The service projects the complete virtual tree into the selected directory; it does not yet preserve the original commit author/message upstream.
- OAuth sessions are held in memory, so a service restart requires users to authorize again.
- Run this behind TLS. The callback-provided temporary password is stored in the Git remote's push URL.

See [SECURITY.md](SECURITY.md) before exposing an instance publicly.

## Container deployment

```sh
docker build -t subgit .
docker run --rm -p 8080:8080 -v "$PWD/data:/data" subgit
```
