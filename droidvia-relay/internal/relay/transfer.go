package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// /v1/transfer is an isolated binary transport for a single Droidvia transfer.
// The relay never examines file contents. The endpoints are responsible for
// authenticated end-to-end encryption, chunk integrity, resume and consent.
//
// This implementation is DEVELOPMENT ONLY: NewDevelopment and its CLI must
// remain loopback-only until device identity and route grants are hardened.
const (
	MaxTransferFrame            = MaxPayloadSize // 256 KiB including AEAD overhead
	maxTransferSessions         = 512
	maxTransferSessionsPerPeer  = 8
	transferJoinTimeout         = 2 * time.Minute
	transferWriteTimeout        = 30 * time.Second
	transferRateBytesPerSecond  = 64 << 20
	transferBurstBytes          = 8 << 20
	transferCloseConflict       websocket.StatusCode = 4009
	transferClosePeerGone       websocket.StatusCode = 4010
	transferCloseJoinTimeout    websocket.StatusCode = 4011
)

var validTransferID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TransferDeviceTicket derives an identity-scoped development bearer token.
// The relay operator provisions it to a device over a trusted channel.
// This is not a replacement for a production device identity or DBP handshake.
func TransferDeviceTicket(devSecret, device string) string {
	mac := hmac.New(sha256.New, []byte(devSecret))
	_, _ = mac.Write([]byte("droidvia v1 transfer device\x00"))
	_, _ = mac.Write([]byte(device))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type transferSession struct {
	a, b  string
	peers map[string]*transferPeer
}

type transferPeer struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	wmu    sync.Mutex
}

func (p *transferPeer) send(typ websocket.MessageType, data []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	ctx, cancel := context.WithTimeout(p.ctx, transferWriteTimeout)
	defer cancel()
	return p.conn.Write(ctx, typ, data)
}

func (p *transferPeer) close(code websocket.StatusCode, message string) {
	_ = p.conn.Close(code, message)
	p.cancel()
}

// transferJoinStatus requires Relay.mu and checks pair identity plus capacity.
func (r *Relay) transferJoinStatus(id, me, other string) int {
	if s := r.transfers[id]; s != nil {
		if !((s.a == me && s.b == other) || (s.b == me && s.a == other)) {
			return http.StatusConflict
		}
		if s.peers[me] != nil {
			return http.StatusConflict
		}
		return 0
	}
	if len(r.transfers) >= maxTransferSessions {
		return http.StatusTooManyRequests
	}
	n := 0
	for _, s := range r.transfers {
		if s.a == me || s.b == me {
			n++
		}
	}
	if n >= maxTransferSessionsPerPeer {
		return http.StatusTooManyRequests
	}
	return 0
}

// GET /v1/transfer?peer=<my peer id>&to=<other peer id>&transfer=<32 hex>
// Authorization: Bearer <TransferDeviceTicket(devSecret, my peer id)>
//
// Both clients must present the same cryptographically random transfer ID and
// reciprocal peer IDs. Only allowlisted pairs may join. All data frames after
// the relay's ready notification are raw, opaque binary WebSocket frames.
func (r *Relay) transfer(w http.ResponseWriter, req *http.Request) {
	me, other, id := req.URL.Query().Get("peer"), req.URL.Query().Get("to"), req.URL.Query().Get("transfer")
	if !validPeerID.MatchString(me) || !validPeerID.MatchString(other) ||
		me == other || !validTransferID.MatchString(id) {
		http.Error(w, "invalid transfer parameters", http.StatusBadRequest)
		return
	}
	if !hmac.Equal([]byte(bearer(req)), []byte(TransferDeviceTicket(r.config.DevToken, me))) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !r.allow[me][other] {
		http.Error(w, "pair is not allowlisted", http.StatusForbidden)
		return
	}

	r.mu.Lock()
	status := r.transferJoinStatus(id, me, other)
	r.mu.Unlock()
	if status != 0 {
		http.Error(w, "transfer room unavailable", status)
		return
	}

	// Keep default websocket origin checks; the development CLI binds only to
	// loopback, and authentication never relies on browser cookies.
	conn, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(MaxTransferFrame)
	ctx, cancel := context.WithCancel(req.Context())
	p := &transferPeer{conn: conn, ctx: ctx, cancel: cancel}
	defer cancel()
	defer conn.CloseNow()

	r.mu.Lock()
	status = r.transferJoinStatus(id, me, other) // close the upgrade race
	if status != 0 {
		r.mu.Unlock()
		p.close(transferCloseConflict, "transfer room unavailable")
		return
	}
	s := r.transfers[id]
	if s == nil {
		s = &transferSession{a: me, b: other, peers: make(map[string]*transferPeer, 2)}
		r.transfers[id] = s
	}
	s.peers[me] = p
	counterpart := s.peers[other]
	r.mu.Unlock()
	defer r.leaveTransfer(id, s, me, p)

	if counterpart == nil {
		timer := time.AfterFunc(transferJoinTimeout, func() {
			r.mu.Lock()
			pending := r.transfers[id] == s && len(s.peers) == 1
			r.mu.Unlock()
			if pending {
				p.close(transferCloseJoinTimeout, "peer did not join")
			}
		})
		defer timer.Stop()
	} else {
		// A sender must wait for ready before transmitting file data.
		// Unlike the existing /v1/connect route, frames have no relay header.
		ready := []byte(`{"event":"ready"}`)
		if err := counterpart.send(websocket.MessageText, ready); err != nil {
			return
		}
		if err := p.send(websocket.MessageText, ready); err != nil {
			return
		}
	}

	r.log.Info("development transfer connected", "peer", me)
	defer r.log.Info("development transfer disconnected", "peer", me)
	pacer := newTransferPacer()
	for {
		typ, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			p.close(websocket.StatusUnsupportedData, "binary data only")
			return
		}
		if len(payload) == 0 {
			p.close(websocket.StatusPolicyViolation, "empty transfer frame")
			return
		}
		if err := pacer.wait(ctx, len(payload)); err != nil {
			return
		}
		r.mu.Lock()
		var destination *transferPeer
		if r.transfers[id] == s {
			destination = s.peers[other]
		}
		r.mu.Unlock()
		if destination == nil || destination.send(websocket.MessageBinary, payload) != nil {
			p.close(transferClosePeerGone, "transfer peer unavailable")
			return
		}
	}
}

// A session is one logical transfer, not a persistent device presence. Closing
// either side invalidates it and closes the other so neither side silently
// loses frames. The endpoints may reconnect with the same ID to resume.
func (r *Relay) leaveTransfer(id string, s *transferSession, me string, p *transferPeer) {
	r.mu.Lock()
	if r.transfers[id] != s || s.peers[me] != p {
		r.mu.Unlock()
		return
	}
	delete(r.transfers, id)
	var other *transferPeer
	for device, candidate := range s.peers {
		if device != me {
			other = candidate
		}
	}
	r.mu.Unlock()
	if other != nil {
		other.close(transferClosePeerGone, "transfer peer disconnected")
	}
}

type transferPacer struct {
	tokens float64
	last   time.Time
}

func newTransferPacer() *transferPacer {
	return &transferPacer{tokens: transferBurstBytes, last: time.Now()}
}

func (p *transferPacer) wait(ctx context.Context, size int) error {
	if size < 0 || size > MaxTransferFrame {
		return errors.New("invalid transfer frame size")
	}
	now := time.Now()
	p.tokens = min(float64(transferBurstBytes),
		p.tokens+now.Sub(p.last).Seconds()*transferRateBytesPerSecond)
	p.last = now
	if p.tokens >= float64(size) {
		p.tokens -= float64(size)
		return nil
	}
	delay := time.Duration((float64(size)-p.tokens)/transferRateBytesPerSecond*float64(time.Second))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		p.tokens = 0
		p.last = time.Now()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
