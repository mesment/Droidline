# Droidvia Relay — Go 1.23.2 standalone module

This is an isolated, **development-only** Go module extracted from the Droidline fork. The main Droidline module remains unchanged at Go 1.26.

## Validate using Go 1.23.2

```sh
cd droidvia-relay
GOTOOLCHAIN=local go version
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build -o ./bin/droidvia-relay ./cmd/droidvia-relay
```

## Local development run

```sh
export DROIDVIA_DEV_RELAY_TOKEN="$(openssl rand -hex 32)"
go run ./cmd/droidvia-relay -listen 127.0.0.1:8767 -pairs phone:home-mac,phone:work-mac
```

Binary WebSocket protocol (development): version(1) | kind(1) | peerId length(1) | peer ID | payload (max 256 KiB).

**NOT PRODUCTION READY. DO NOT DEPLOY PUBLICLY.** Shared development token alone does not authenticate each individual peer. Stable identity proof, route authorization and end-to-end DBP session encryption are not yet implemented. The CLI intentionally only accepts loopback addresses. Files and media flows require further integration/validation.

Source inspiration: MIT-licensed KnifeLemon/Droidline, preserving copyright in LICENSE.
