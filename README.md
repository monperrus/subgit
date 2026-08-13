# subgit

`subgit` exposes any public GitHub directory as a normal, read-only Git smart-HTTP repository. It builds a filtered bare repository, so clients use ordinary Git commands. The URL contains one identifier: `OWNER/REPOSITORY/FOLDER`.

```sh
git clone https://HOST/labri-progress/what-are-they-doing/paper.git
git clone https://HOST/monperrus/test-repo-public/.github.git
```

Copy `config.example.json` to a persistent data directory and run `subgit`. Requested GitHub directories are cached and refreshed periodically. The virtual history contains commits that affect the requested path, with that path removed from the checkout root.

## Local run

```sh
mkdir -p data
cp config.example.json data/config.json
SUBGIT_CONFIG=$PWD/data/config.json go run .
git clone http://localhost:8080/paper.git
```

`GET /status` reports the most recent successful materialization. Pushes are deliberately rejected in this MVP.

## GitHub OAuth App setup

Register a GitHub OAuth App with callback URL `https://HOST/auth/github/callback`, then provide `GITHUB_OAUTH_CLIENT_ID`, `GITHUB_OAUTH_CLIENT_SECRET`, and `SUBGIT_PUBLIC_URL=https://HOST` as service environment variables. Users begin authorization at:

```text
https://HOST/auth/github?return_to=/OWNER/REPOSITORY/FOLDER.git
```

The callback creates an eight-hour opaque Git HTTPS password. Write-through handling uses that password to identify the user without persisting their GitHub access token; the repository-write transport is the next integration point.

## Container deployment

```sh
docker build -t subgit .
docker run --rm -p 8080:8080 -v "$PWD/data:/data" subgit
```
