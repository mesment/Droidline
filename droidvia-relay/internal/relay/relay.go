// Package relay provides a development-only binary WebSocket relay for Droidvia.
//
// This package deliberately does not authenticate Droidvia device identities or
// encrypt DBP payloads. Never expose it to the Internet: R03 will replace the
// temporary shared-token authentication with authenticated peer/route grants.
package relay

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	// ProtocolVersion is the first development relay-envelope version.
	ProtocolVersion byte = 1
	kindData        byte = 1
	MaxPayloadSize       = 256 << 10 // 256 KiB per WebSocket message
	MaxPeerIDSize        = 128
	defaultMaxPeers      = 32
	writeTimeout         = 5 * time.Second
)

var (
	validPeerID  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	ErrBadFrame  = errors.New("invalid Droidvia relay frame")
	ErrBadPeerID = errors.New("invalid Droidvia relay peer ID")
)

type Pair struct {
	A string
	B string
}

// Config is intentionally temporary: development peers all know the same
// DevToken. Pairs explicitly limit destinations, but this is NOT production
// authentication; a peer possessing DevToken can impersonate any peer ID.
type Config struct {
	DevToken string
	Pairs    []Pair
	MaxPeers int
}

type Relay struct {
	config Config
	log    *slog.Logger
	mu     sync.Mutex
	peers  map[string]*peer
	allow  map[string]map[string]bool
	transfers map[string]*transferSession
}

type peer struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	wmu    sync.Mutex
}

func (p *peer) send(frame []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	ctx, cancel := context.WithTimeout(p.ctx, writeTimeout)
	defer cancel()
	return p.conn.Write(ctx, websocket.MessageBinary, frame)
}

func (p *peer) close() {
	p.cancel()
	p.conn.CloseNow()
}

// NewDevelopment refuses to start without a strong development token and an
// explicit allowlist of peer pairs. The CLI additionally binds to loopback.
func NewDevelopment(cfg Config, log *slog.Logger) (*Relay, error) {
	if len(cfg.DevToken) < 32 {
		return nil, errors.New("DevToken must be at least 32 characters")
	}
	if len(cfg.Pairs) == 0 {
		return nil, errors.New("at least one development peer pair is required")
	}
	if cfg.MaxPeers == 0 {
		cfg.MaxPeers = defaultMaxPeers
	}
	if cfg.MaxPeers < 2 || cfg.MaxPeers > 1024 {
		return nil, errors.New("MaxPeers must be between 2 and 1024")
	}
	if log == nil {
		log = slog.Default()
	}
	r := &Relay{
		config: cfg,
		log:    log,
		peers:  make(map[string]*peer),
		allow:  make(map[string]map[string]bool),
		transfers: make(map[string]*transferSession),
	}
	for _, pair := range cfg.Pairs {
		if !validPeerID.MatchString(pair.A) || !validPeerID.MatchString(pair.B) || pair.A == pair.B {
			return nil, fmt.Errorf("invalid development peer pair %q:%q", pair.A, pair.B)
		}
		if r.allow[pair.A] == nil {
			r.allow[pair.A] = make(map[string]bool)
		}
		if r.allow[pair.B] == nil {
			r.allow[pair.B] = make(map[string]bool)
		}
		r.allow[pair.A][pair.B] = true
		r.allow[pair.B][pair.A] = true
	}
	return r, nil
}

func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/connect", r.connect)
	mux.HandleFunc("GET /v1/transfer", r.transfer)
	return mux
}

func bearer(req *http.Request) string {
	v, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return v
}

func (r *Relay) connect(w http.ResponseWriter, req *http.Request) {
	id := req.URL.Query().Get("peer")
	if !validPeerID.MatchString(id) {
		http.Error(w, "invalid peer ID", http.StatusBadRequest)
		return
	}
	token := bearer(req)
	if subtle.ConstantTimeCompare([]byte(token), []byte(r.config.DevToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, ok := r.allow[id]; !ok {
		http.Error(w, "peer is not in development allowlist", http.StatusForbidden)
		return
	}

	// The development CLI is loopback-only. Native clients do not need a
	// browser Origin bypass; use websocket.Accept's default origin checks.
	r.mu.Lock()
	old := r.peers[id]
	if old == nil && len(r.peers) >= r.config.MaxPeers {
		r.mu.Unlock()
		http.Error(w, "relay is full", http.StatusServiceUnavailable)
		return
	}
	r.mu.Unlock()

	conn, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(MaxPayloadSize + MaxPeerIDSize + 3)
	ctx, cancel := context.WithCancel(req.Context())
	p := &peer{conn: conn, ctx: ctx, cancel: cancel}

	r.mu.Lock()
	old = r.peers[id]
	r.peers[id] = p
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
	r.log.Info("development peer connected", "peer", id)

	defer func() {
		r.mu.Lock()
		if r.peers[id] == p {
			delete(r.peers, id)
		}
		r.mu.Unlock()
		p.close()
		r.log.Info("development peer disconnected", "peer", id)
	}()

	for {
		typ, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			_ = conn.Close(websocket.StatusUnsupportedData, "binary frames only")
			return
		}
		destID, payload, err := DecodeDataFrame(b)
		if err != nil {
			_ = conn.Close(websocket.StatusInvalidFramePayloadData, "invalid relay frame")
			return
		}

		r.mu.Lock()
		allowed := r.allow[id][destID]
		target := r.peers[destID]
		r.mu.Unlock()
		if !allowed || target == nil {
			// Do not reveal which unapproved destinations are online.
			continue
		}
		forwarded, err := EncodeDataFrame(id, payload)
		if err != nil {
			return
		}
		if err := target.send(forwarded); err != nil {
			target.close() // the recipient will be removed by its own handler
		}
	}
}

// EncodeDataFrame encodes a receiver-addressed frame on send. When relaying,
// the destination field is replaced by the authenticated sender's ID.
// Frame v1: version(1) | kind(1) | peerIdLen(1) | peerId(N) | payload.
func EncodeDataFrame(peerID string, payload []byte) ([]byte, error) {
	if !validPeerID.MatchString(peerID) {
		return nil, ErrBadPeerID
	}
	if len(payload) > MaxPayloadSize {
		return nil, ErrBadFrame
	}
	b := make([]byte, 3+len(peerID)+len(payload))
	b[0], b[1], b[2] = ProtocolVersion, kindData, byte(len(peerID))
	copy(b[3:], peerID)
	copy(b[3+len(peerID):], payload)
	return b, nil
}

func DecodeDataFrame(b []byte) (peerID string, payload []byte, err error) {
	if len(b) < 4 || b[0] != ProtocolVersion || b[1] != kindData {
		return "", nil, ErrBadFrame
	}
	n := int(b[2])
	if n == 0 || n > MaxPeerIDSize || len(b) < 3+n || len(b)-(3+n) > MaxPayloadSize {
		return "", nil, ErrBadFrame
	}
	id := string(b[3 : 3+n])
	if !validPeerID.MatchString(id) {
		return "", nil, ErrBadPeerID
	}
	return id, b[3+n:], nil
}
