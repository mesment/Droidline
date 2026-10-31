package relay

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testTransferID = "0123456789abcdef0123456789abcdef"

func newTransferServer(t *testing.T) *httptest.Server {
	t.Helper()
	r, err := NewDevelopment(Config{
		DevToken: devToken,
		Pairs: []Pair{
			{A: "phone", B: "mac"},
			{A: "phone", B: "pc"},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(r.Handler())
	t.Cleanup(s.Close)
	return s
}

func transferURL(base, me, other, transferID string) string {
	u, _ := url.Parse(base)
	u.Scheme = "ws"
	u.Path = "/v1/transfer"
	q := url.Values{}
	q.Set("peer", me)
	q.Set("to", other)
	q.Set("transfer", transferID)
	u.RawQuery = q.Encode()
	return u.String()
}

func dialTransfer(t *testing.T, base, me, other, id, ticket string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, transferURL(base, me, other, id),
		&websocket.DialOptions{HTTPHeader: http.Header{
			"Authorization": {"Bearer " + ticket},
		}})
}

func mustConnectTransfer(t *testing.T, base, me, other, id string) *websocket.Conn {
	t.Helper()
	c, resp, err := dialTransfer(t, base, me, other, id, TransferDeviceTicket(devToken, me))
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("connect %s: status=%d err=%v", me, status, err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func expectTransferStatus(t *testing.T, base, me, other, id, ticket string, want int) {
	t.Helper()
	c, resp, err := dialTransfer(t, base, me, other, id, ticket)
	if c != nil {
		c.CloseNow()
	}
	if err == nil || resp == nil || resp.StatusCode != want {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("expected HTTP %d, got %d (%v)", want, status, err)
	}
}

func expectTransferReady(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, msg, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("waiting for transfer ready: %v", err)
	}
	if typ != websocket.MessageText || string(msg) != `{"event":"ready"}` {
		t.Fatalf("unexpected ready: type=%v msg=%q", typ, msg)
	}
}

func sendTransferBytes(t *testing.T, c *websocket.Conn, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, data); err != nil {
		t.Fatal(err)
	}
}

func receiveTransferBytes(t *testing.T, c *websocket.Conn, expected []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, received, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("binary read: %v", err)
	}
	if typ != websocket.MessageBinary || !bytes.Equal(received, expected) {
		t.Fatalf("wrong frame type=%v, len=%d, expected=%d", typ, len(received), len(expected))
	}
}

func TestTransferRawBinaryBidirectional(t *testing.T) {
	s := newTransferServer(t)
	mac := mustConnectTransfer(t, s.URL, "mac", "phone", testTransferID)
	phone := mustConnectTransfer(t, s.URL, "phone", "mac", testTransferID)
	expectTransferReady(t, mac)
	expectTransferReady(t, phone)

	payload := append([]byte{0, 255, 254, 0, 128}, bytes.Repeat([]byte{0xca, 0xfe}, 64*1024)...)
	sendTransferBytes(t, mac, payload)
	receiveTransferBytes(t, phone, payload)

	reply := []byte{1, 2, 0, 255}
	sendTransferBytes(t, phone, reply)
	receiveTransferBytes(t, mac, reply)
}

func TestTransferRejectsInvalidIdentityPairAndTicket(t *testing.T) {
	s := newTransferServer(t)
	bad := []struct {
		me, other, id, token string
		want                 int
	}{
		{"mac", "phone", testTransferID, "", http.StatusUnauthorized},
		{"mac", "phone", testTransferID, devToken, http.StatusUnauthorized},
		{"mac", "phone", testTransferID, TransferDeviceTicket(devToken, "phone"), http.StatusUnauthorized},
		{"mac", "phone", "bad", "anything", http.StatusBadRequest},
		{"mac", "mac", testTransferID, "anything", http.StatusBadRequest},
		{"mac", "unknown", testTransferID, TransferDeviceTicket(devToken, "mac"), http.StatusForbidden},
		{"bad!peer", "phone", testTransferID, "anything", http.StatusBadRequest},
		{"mac", "phone", strings.Repeat("f", 31), "anything", http.StatusBadRequest},
	}
	for _, tc := range bad {
		expectTransferStatus(t, s.URL, tc.me, tc.other, tc.id, tc.token, tc.want)
	}
	if TransferDeviceTicket(devToken, "phone") == TransferDeviceTicket(devToken, "mac") {
		t.Fatal("device-scoped tickets must differ")
	}
}

func TestTransferPairIsReciprocalAndUnique(t *testing.T) {
	s := newTransferServer(t)
	mac := mustConnectTransfer(t, s.URL, "mac", "phone", testTransferID)

	expectTransferStatus(t, s.URL, "mac", "phone", testTransferID,
		TransferDeviceTicket(devToken, "mac"), http.StatusConflict)
	expectTransferStatus(t, s.URL, "phone", "pc", testTransferID,
		TransferDeviceTicket(devToken, "phone"), http.StatusConflict)

	phone := mustConnectTransfer(t, s.URL, "phone", "mac", testTransferID)
	expectTransferReady(t, mac)
	expectTransferReady(t, phone)
}

func TestTransferNoSilentDropWhenPeerAbsent(t *testing.T) {
	s := newTransferServer(t)
	mac := mustConnectTransfer(t, s.URL, "mac", "phone", testTransferID)
	sendTransferBytes(t, mac, []byte{1, 3, 3, 7})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := mac.Read(ctx)
	if err == nil || websocket.CloseStatus(err) != transferClosePeerGone {
		t.Fatalf("expected missing-peer close code %d, got %v", transferClosePeerGone, err)
	}
}

func TestTransferDisconnectAndResumeSameID(t *testing.T) {
	s := newTransferServer(t)
	mac := mustConnectTransfer(t, s.URL, "mac", "phone", testTransferID)
	phone := mustConnectTransfer(t, s.URL, "phone", "mac", testTransferID)
	expectTransferReady(t, mac)
	expectTransferReady(t, phone)
	mac.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := phone.Read(ctx)
	if err == nil {
		t.Fatal("other peer did not disconnect")
	}
	macAgain := mustConnectTransfer(t, s.URL, "mac", "phone", testTransferID)
	phoneAgain := mustConnectTransfer(t, s.URL, "phone", "mac", testTransferID)
	expectTransferReady(t, macAgain)
	expectTransferReady(t, phoneAgain)
	sendTransferBytes(t, phoneAgain, []byte{0xaa, 0xbb})
	receiveTransferBytes(t, macAgain, []byte{0xaa, 0xbb})
}

func TestTransferRejectsTextAndOversizedMessages(t *testing.T) {
	s := newTransferServer(t)
	mac := mustConnectTransfer(t, s.URL, "mac", "phone", testTransferID)
	phone := mustConnectTransfer(t, s.URL, "phone", "mac", testTransferID)
	expectTransferReady(t, mac)
	expectTransferReady(t, phone)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mac.Write(ctx, websocket.MessageText, []byte("plaintext")); err != nil {
		t.Fatal(err)
	}
	_, _, err := mac.Read(ctx)
	if err == nil || websocket.CloseStatus(err) != websocket.StatusUnsupportedData {
		t.Fatalf("expected binary-only close: %v", err)
	}
	// A separate session ensures the over-limit sender cannot affect the
	// original session or any other peer pairing.
	const anotherID = "fedcba9876543210fedcba9876543210"
	mac2 := mustConnectTransfer(t, s.URL, "mac", "phone", anotherID)
	phone2 := mustConnectTransfer(t, s.URL, "phone", "mac", anotherID)
	expectTransferReady(t, mac2)
	expectTransferReady(t, phone2)
	_ = mac2.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte{0}, MaxTransferFrame+1))
	_, _, err = mac2.Read(ctx)
	if err == nil {
		t.Fatal("oversized message did not close")
	}
}

func TestTransferTwoAllowedPairsAreIsolated(t *testing.T) {
	s := newTransferServer(t)
	mac := mustConnectTransfer(t, s.URL, "mac", "phone", testTransferID)
	phone := mustConnectTransfer(t, s.URL, "phone", "mac", testTransferID)
	expectTransferReady(t, mac)
	expectTransferReady(t, phone)
	// Even phone->pc, which is allowlisted, cannot hijack the existing ID.
	expectTransferStatus(t, s.URL, "pc", "phone", testTransferID,
		TransferDeviceTicket(devToken, "pc"), http.StatusConflict)
	const next = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pc := mustConnectTransfer(t, s.URL, "pc", "phone", next)
	phone2 := mustConnectTransfer(t, s.URL, "phone", "pc", next)
	expectTransferReady(t, pc)
	expectTransferReady(t, phone2)
	sendTransferBytes(t, mac, []byte{1})
	sendTransferBytes(t, pc, []byte{2})
	receiveTransferBytes(t, phone, []byte{1})
	receiveTransferBytes(t, phone2, []byte{2})
}
