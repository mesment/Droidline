# Droidvia Relay development branch

This branch adds **an isolated development-only relay** alongside the original Droidline relay. No existing Droidline client/server behavior has been changed.

## Status

- ✅ Binary WebSocket frame with a version, target peer ID and opaque payload.
- ✅ One Android peer ↔ multiple desktop peers through explicit *development* pair allowlists.
- ✅ Max 256 KiB binary frames, connection replacement, write timeout, max connection count.
- ✅ `/healthz` and `/v1/connect?peer=<id>`.
- 🚫 **NOT Internet-ready.** Development access uses a shared token: anyone with the token could impersonate a peer. No cryptographic Droidvia identity proof or session E2E key exchange is implemented yet.
- 🚫 No durable pending queues, cross-instance routing, WebRTC fallback or user-facing remote feature.

## Run locally

Use Go 1.26 as required by the root `go.mod`.

```sh
export DROIDVIA_DEV_RELAY_TOKEN="$(openssl rand -hex 32)"
go run ./server/cmd/droidvia-relay \
  -listen 127.0.0.1:8767 \
  -pairs phone:home-mac,phone:work-mac
```

Use a WebSocket client with `Authorization: Bearer <dev token>` to connect to `ws://127.0.0.1:8767/v1/connect?peer=phone`, etc.

Frame format:

```text
version(1) | kind(1) | peerIdLen(1) | peerId(N) | payload(<=256 KiB)
```

The client sends a destination peer ID. The relay replaces it with the source ID in the forwarded frame, so recipients learn who sent the bytes. These are **transport frames, not DBP wire-format frames**.

## Next stages

1. Bind relay connections to cryptographically proved Droidvia stable identities.
2. Authorize route creation using pairing grants or short-lived relay credentials.
3. Establish authenticated end-to-end DBP sessions; relay must not possess decrypted payloads.
4. Integrate with Android Gateway and Mac `ConnectionSupervisor`.
5. Add flow control/backpressure and deployment hardening before public exposure.

Keep the original MIT license and attribution to KnifeLemon. Source: `server/internal/relay/relay.go`, original fork of `KnifeLemon/Droidline`.
