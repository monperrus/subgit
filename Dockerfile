FROM golang:1.21-bookworm AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go build -trimpath -ldflags='-s -w' -o /out/subgit .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends git git-filter-repo ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/subgit /usr/local/bin/subgit
COPY config.example.json /data/config.json
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/subgit"]
