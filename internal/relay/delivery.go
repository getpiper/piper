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
	"log"
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

// maxPendingPerAgent bounds the parked-event table for one box. A PR-heavy repo
// creates one slot per open PR, so the cap is what stops an offline box from
// growing the table without limit. Oldest slots are evicted first.
const maxPendingPerAgent = 50

// PendingEvent is a webhook parked for a box that was offline when it arrived.
type PendingEvent struct {
	AgentName string
	App       string
	Ref       string
	Event     string
	Payload   []byte
	CreatedAt string
}

// ParkEvent stores an undelivered event, coalescing by (agent, app, ref): a
// newer event for the same ref replaces the older one. Deploys are
// last-write-wins, so replaying intermediate commits on reconnect would be
// actively wrong — a box that was off overnight should deploy the tip, once.
// pendingTimeLayout is fixed-width, unlike RFC3339Nano (which trims trailing
// fractional zeros, so "…:05Z" sorts lexicographically *after* "…:05.4Z").
// The eviction and drain ordering below compare created_at as strings and
// depend on lexicographic == chronological.
const pendingTimeLayout = "2006-01-02T15:04:05.000000000Z"

func (s *Store) ParkEvent(agentName, app, ref, event string, payload []byte) error {
	now := time.Now().UTC().Format(pendingTimeLayout)
	if _, err := s.db.Exec(
		`INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(agent_name, app, ref) DO UPDATE SET
		     event = excluded.event, payload = excluded.payload, created_at = excluded.created_at`,
		agentName, app, ref, event, payload, now); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`DELETE FROM pending_events
		  WHERE agent_name = ?
		    AND rowid NOT IN (
		        SELECT rowid FROM pending_events WHERE agent_name = ?
		         ORDER BY created_at DESC LIMIT ?)`,
		agentName, agentName, maxPendingPerAgent)
	return err
}

// ReparkEvent restores a replay that failed delivery, using the event's
// ORIGINAL created_at rather than now: a re-park is not a new arrival, and
// must not win the coalescing slot by park recency when a genuinely newer
// event took the slot while the replay was in flight (DrainFor drains,
// delivers, and re-parks in three separate steps, so that window is real).
// The WHERE clause makes the overwrite conditional on the re-park still
// being the newest thing for this ref; if it lost the race, the INSERT is
// silently dropped, which is correct by design — the newer event already
// in the slot supersedes it under last-write-wins.
func (s *Store) ReparkEvent(agentName, app, ref, event string, payload []byte, createdAt string) error {
	_, err := s.db.Exec(
		`INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(agent_name, app, ref) DO UPDATE SET
		     event = excluded.event, payload = excluded.payload, created_at = excluded.created_at
		 WHERE excluded.created_at > pending_events.created_at`,
		agentName, app, ref, event, payload, createdAt)
	return err
}

// DrainEvents returns and removes every parked event for agentName. Read and
// delete share one immediate transaction (see Open's _txlock): a concurrent
// ParkEvent either commits before it and is returned, or blocks until after
// the delete and survives for the next drain — the blanket DELETE can never
// destroy a row this call did not return.
func (s *Store) DrainEvents(agentName string) ([]PendingEvent, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT app, ref, event, payload, created_at FROM pending_events
		  WHERE agent_name=? ORDER BY created_at`, agentName)
	if err != nil {
		return nil, err
	}
	var out []PendingEvent
	for rows.Next() {
		ev := PendingEvent{AgentName: agentName}
		if err := rows.Scan(&ev.App, &ev.Ref, &ev.Event, &ev.Payload, &ev.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM pending_events WHERE agent_name=?`, agentName); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// DrainFor replays every parked event for agentName. Called on reconnect, and
// after a park to close the race where the box came back between a delivery
// failing and the park landing. It bails while the agent is offline — the
// destructive drain must not run when nothing can be delivered — and a replay
// that fails is re-parked, never dropped: GitHub already got its 202, so a
// lost event here would never be retried by anyone.
func (t *TunnelDelivery) DrainFor(ctx context.Context, agentName string) {
	if _, ok := t.router.Lookup(agentName); !ok {
		return // events stay parked for the reconnect drain
	}
	events, err := t.st.DrainEvents(agentName)
	if err != nil {
		log.Printf("relay: drain pending for %s: %v", agentName, err)
		return
	}
	for _, ev := range events {
		b := Binding{AgentName: ev.AgentName, App: ev.App}
		if err := t.Deliver(ctx, b, ev.Event, ev.Payload); err != nil {
			log.Printf("relay: replay %s to %s/%s: %v (re-parking)", ev.Event, ev.AgentName, ev.App, err)
			if perr := t.st.ReparkEvent(ev.AgentName, ev.App, ev.Ref, ev.Event, ev.Payload, ev.CreatedAt); perr != nil {
				log.Printf("relay: re-park %s for %s/%s: %v", ev.Event, ev.AgentName, ev.App, perr)
			}
		}
	}
}
