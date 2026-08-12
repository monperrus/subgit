# subgit

`subgit` exposes a directory in a monorepo as a normal, read-only Git smart-HTTP repository. It builds a filtered bare repository, so clients use ordinary Git commands:

```sh
git clone https://HOST/paper.git
```

Copy `config.example.json` to a persistent data directory and run `subgit`. Each configured repository is refreshed periodically. The virtual history contains commits that affect its configured path, with that path removed from the checkout root.

## Local run

```sh
mkdir -p data
cp config.example.json data/config.json
SUBGIT_CONFIG=$PWD/data/config.json go run .
git clone http://localhost:8080/paper.git
```

`GET /status` reports the most recent successful materialization. Pushes are deliberately rejected in this MVP.

## Container deployment

```sh
docker build -t subgit .
docker run --rm -p 8080:8080 -v "$PWD/data:/data" subgit
```
