package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// ErrAgentOffline is returned when a bound agent has no live tunnel session.
var ErrAgentOffline = errors.New("agent not connected")

// deliveryTimeout bounds one delivery attempt end to end.
const deliveryTimeout = 30 * time.Second

// TunnelDelivery hands a verified webhook to a box over its tunnel. It opens a
// KindHTTP stream — which the agent already pipes to Caddy on :80 — and speaks
// plain HTTP with Host hooks.<base>, so the box's existing webhook listener and
// Caddy route serve it exactly as a public request. Nothing new is exposed to
// the internet.
type TunnelDelivery struct {
	st     *Store
	router *Router
}

func NewTunnelDelivery(st *Store, router *Router) *TunnelDelivery {
	return &TunnelDelivery{st: st, router: router}
}

func (t *TunnelDelivery) Deliver(ctx context.Context, b Binding, eventType string, payload []byte) error {
	sess, ok := t.router.Lookup(b.AgentName)
	if !ok {
		return ErrAgentOffline
	}
	secret, err := t.st.AgentWebhookSecret(b.AgentName)
	if err != nil {
		return err
	}

	stream, err := sess.OpenKind(tunnel.KindHTTP)
	if err != nil {
		return fmt.Errorf("open delivery stream: %w", err)
	}
	defer stream.Close()
	deadline := time.Now().Add(deliveryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = stream.SetDeadline(deadline)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://hooks."+b.AgentName+"/", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", eventType)
	// GitHub's own signature is never forwarded: the box shares no secret with
	// GitHub in brokered mode. Re-sign with the per-agent secret so the agent's
	// existing verification path is unchanged and the tunnel is not treated as
	// authenticating on its own.
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, payload))
	req.ContentLength = int64(len(payload))

	if err := req.Write(stream); err != nil {
		return fmt.Errorf("write delivery: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(stream), req)
	if err != nil {
		return fmt.Errorf("read delivery response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("box rejected delivery: %s", resp.Status)
	}
	return nil
}

func signPayload(secret string, payload []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(payload)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}
