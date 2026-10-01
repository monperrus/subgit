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

Requested GitHub directories are cached. Their virtual history contains commits that affect the selected path, with that path removed from the checkout root.

## Freshness guarantee

subgit never advertises stale refs. Every clone, fetch and push starts with a ref advertisement (`info/refs`). At that point subgit runs `git ls-remote` against GitHub and compares the upstream head with the commit the cache was built from. That commit is recorded in `subgit-upstream` inside each virtual repository.

- Heads match: the cache is served. This adds one `ls-remote` round trip, about 0.5 s.
- Upstream moved: the request waits up to `refresh_wait` for the rebuild. If the rebuild takes longer, the request fails with `503 Service Unavailable`, `Retry-After: 10` and the message `upstream moved; the virtual repository is being rebuilt, retry shortly`. Rerun the Git command.
- GitHub unreachable: the request fails with `503` and the message `cannot verify freshness against upstream`. subgit does not fall back to the cache.

Freshness does not depend on time or on process state. A restart, a failed earlier refresh, or a cache built by an older subgit without `subgit-upstream` all trigger a rebuild on the next request. A background poller rebuilds known repositories every `refresh_interval`, so the rebuild usually happens before anyone fetches.

## Local run

```sh
mkdir -p data
cp config.example.json data/config.json
SUBGIT_CONFIG=$PWD/data/config.json SUBGIT_DATA_DIR=$PWD/data go run .
git clone http://localhost:8080/monperrus/test-repo-public/.github.git
```

Configuration keys (all optional):

- `listen` (default `:8080`)
- `data_dir` (default `/data`; overridden by `SUBGIT_DATA_DIR`)
- `refresh_interval` (default `1m`): background poll period. It only affects latency, never correctness.
- `refresh_wait` (default `30s`): how long a Git request may wait for a rebuild before receiving a retryable 503. Keep it below your reverse proxy's read timeout.
- `sync_timeout` (default `30m`): kills a materialization that hangs, so it cannot block later refreshes.

`GET /status` reports, per repository:

- `upstream`: the GitHub head at the last check.
- `served`: the upstream commit the cache was built from.
- `last_check`, `last_sync`, `refreshing`, `error`.

The cache is fresh exactly when `served == upstream`. To check from outside, compare the two heads; the first command prints the virtual head, the second the GitHub head:

```sh
curl -s https://subgit.gakoy.com/status | jq '."OWNER/REPOSITORY/FOLDER"'
git ls-remote https://subgit.gakoy.com/OWNER/REPOSITORY/FOLDER.git refs/heads/main
git ls-remote https://github.com/OWNER/REPOSITORY.git refs/heads/main
```

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
- A virtual push succeeds only after its upstream projection succeeds. If GitHub rejects the projection—for example because the upstream branch moved—the virtual branch is rolled back and Git reports a conflict.
- The service projects the complete virtual tree into the selected directory. The upstream commit keeps the virtual commit's author, committer, dates and message, so the rebuilt virtual repository reproduces the pushed commit hash. One exception: subgit cannot reproduce a GPG/SSH signature. A signed pushed commit is therefore replaced by an unsigned commit with the same content, and the pusher must run `git pull --rebase`, or disable commit signing for the clone.
- Every rebuild re-filters the full upstream history. For a large repository, a fetch right after an upstream commit can receive the retryable 503 until the rebuild completes. On this instance a full rebuild of `labri-progress/what-are-they-doing` took about 2 minutes.
- OAuth sessions are held in memory, so a service restart requires users to authorize again.
- Run this behind TLS. The callback-provided temporary password is stored in the Git remote's push URL.

See [SECURITY.md](SECURITY.md) before exposing an instance publicly.

## Container deployment

```sh
docker build -t subgit .
docker run --rm -p 8080:8080 -v "$PWD/data:/data" subgit
```
