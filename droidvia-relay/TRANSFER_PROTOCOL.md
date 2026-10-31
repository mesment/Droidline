# Droidvia binary transfer relay — v1 (development)

This document describes the **Go 1.23.2 standalone relay** in `droidvia-relay/`.
It does not modify the original Droidline phone-automation routes.
This is a **transport implementation**, not yet a complete, secure file-transfer
application. Do **not** expose the development CLI to the public Internet.

## Connection and provisioning

Start the local development relay:

```sh
cd droidvia-relay
export DROIDVIA_DEV_RELAY_TOKEN="$(openssl rand -hex 32)"
go run ./cmd/droidvia-relay -listen 127.0.0.1:8767 -pairs phone:mac
```

The operator provisions **different** development tickets for each device:

```sh
go run ./cmd/droidvia-relay -ticket-for phone
go run ./cmd/droidvia-relay -ticket-for mac
```

Keep tickets secret and exchange transfer IDs through a separately authenticated
device-pairing/control channel. The relay only knows the configured allowlist
pairs and HMAC-derived development credentials. It cannot prove physical device
identity or consent. Neither the `DROIDVIA_DEV_RELAY_TOKEN` nor tickets should
be placed in a URL or sent in logs.

Each peer connects:

```http
GET /v1/transfer?peer=phone&to=mac&transfer=2f9a021e8ca14e26b993ed3badb20d42 HTTP/1.1
Connection: Upgrade
Upgrade: websocket
Authorization: Bearer <phone's transfer device ticket>
```

The peer `mac` connects with reciprocal `peer=mac&to=phone` using the **same
transfer ID** and its own ticket. The ID MUST be 16 cryptographically random
bytes encoded as 32 lowercase hexadecimal characters; never reuse IDs between
unrelated transfers. The relay requires reciprocal IDs and an allowlisted pair.

When both are connected, each receives:

```json
{"event":"ready"}
```

The clients MUST wait for this event before sending file data.

## Binary data path

Each post-ready WebSocket data message MUST be a **Binary Frame** with up to
`256 KiB` of bytes. Unlike `/v1/connect`, there is **no relay-specific
header**: all bytes pass through unchanged and in the order received, and the
recipient receives a binary frame with identical content. Text frames from
clients, empty frames, and oversized messages are rejected.

Clients MUST send **authenticated end-to-end encrypted ciphertext** here.
TLS/WSS alone is insufficient because the relay server can terminate TLS.
The current Go relay does not and cannot enforce the encryption properties of
opaque payloads. The Droidvia application-layer protocol is responsible for:

- Mutual identity verification and user consent (QR/verified pair identity).
- AEAD framing with authenticated transfer ID, file ID, sequence, offsets and
  file metadata; replay protection, secure nonce generation and key rotation.
- Explicit CHUNK / ACK / RESUME / COMPLETE / CANCEL semantics and integrity
  verification (hash/checksum).
- Durable receiving state, partial-file recovery and atomic finalization.
- Retry / resume after a disconnect, and a deliberate backpressure strategy.

The relay forwards one incoming binary message to the other device at a time.
It does not buffer entire files or persist bytes. There is no disk storage or
HTTP upload endpoint. Per-sender shaping is 64 MiB/s with an 8 MiB burst
(current development constant). A maximum of 512 sessions exist globally,
and one device can participate in at most 8 sessions.

If the peer is absent and data is sent, or if an active peer disconnects,
the sender is closed with code `4010`; the client may reconnect using the
same transfer ID and its own **application-level acknowledged offset**.
Unpaired sessions time out after two minutes (`4011`).

### Close/error codes

| Code | Reason |
| --- | --- |
| 1003 | Text frame unsupported; binary data only |
| 1008 | Empty data frame |
| 1009 | Oversized incoming message (WebSocket library) |
| 4009 | Conflicting session / duplicate peer during concurrent upgrade |
| 4010 | Peer absent or disconnected |
| 4011 | Peer did not join in two minutes |

HTTP handshake errors use 400 for invalid input, 401 for missing/incorrect
device ticket, 403 for non-allowlisted pairs, 409 for conflicting session IDs
and 429 when relay session capacity is exhausted.

## Go 1.23.2 validation

```sh
cd droidvia-relay
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build ./cmd/droidvia-relay
```

The included integration tests exercise real HTTP WebSocket upgrades,
authorization, raw binary payload preservation, session isolation, disconnects,
rejoin and non-binary rejection.

## Required before a public release (R03+)

The CLI remains strictly loopback-bound. Before enabling Internet deployment,
replace development tickets and static allowlists with stable, verified device
identities; signed short-lived **pair/transfer grants**, anti-replay controls
and revocation; validate cross-device E2EE with test vectors; configure WSS
certificate verification, server-wide rate limits and observability; and add
real Android/Desktop transfer clients with interrupted-transfer tests.

`/v1/connect` remains unchanged and must not be mistaken for the new
`/v1/transfer` endpoint.
