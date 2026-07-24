package relay

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

func TestDeliverySignsWithAgentSecretAndDropsGitHubs(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)

	secret, err := st.AgentWebhookSecret(base)
	if err != nil {
		t.Fatalf("AgentWebhookSecret: %v", err)
	}
	if secret == "" {
		t.Fatal("enrollment minted no webhook secret")
	}

	// Stand in for the box: accept the KindHTTP stream and answer 202.
	type got struct {
		host, sig, ghSig, event string
		body                    []byte
	}
	ch := make(chan got, 1)
	go func() {
		kind, conn, err := sess.AcceptKind()
		if err != nil || kind != tunnel.KindHTTP {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		body, _ := io.ReadAll(req.Body)
		ch <- got{
			host:  req.Host,
			sig:   req.Header.Get("X-Hub-Signature-256"),
			ghSig: req.Header.Get("X-Hub-Signature"),
			event: req.Header.Get("X-GitHub-Event"),
			body:  body,
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
	}()

	d := NewTunnelDelivery(st, router)
	payload := []byte(`{"ref":"refs/heads/main"}`)
	b := Binding{AgentName: base, App: "blog", Repo: "alice/blog", Branch: "main"}
	if err := d.Deliver(context.Background(), b, "push", payload); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	select {
	case g := <-ch:
		if g.host != "hooks."+base {
			t.Fatalf("Host = %q, want hooks.%s", g.host, base)
		}
		if g.event != "push" {
			t.Fatalf("event = %q", g.event)
		}
		if string(g.body) != string(payload) {
			t.Fatalf("body = %q", g.body)
		}
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(payload)
		want := "sha256=" + hex.EncodeToString(m.Sum(nil))
		if g.sig != want {
			t.Fatalf("signature = %q, want %q", g.sig, want)
		}
		if g.ghSig != "" {
			t.Fatal("GitHub's original signature was forwarded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no request arrived on the tunnel")
	}
}

func TestDeliveryOfflineAgent(t *testing.T) {
	st := openTestStore(t)
	_, base := enrolledAgent(t, st, "1001", "alice")
	d := NewTunnelDelivery(st, NewRouter())

	err := d.Deliver(context.Background(), Binding{AgentName: base, App: "blog"}, "push", []byte(`{}`))
	if !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("err = %v, want ErrAgentOffline", err)
	}
}

func TestDrainForReplaysOnlyTheNewestPerRef(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"new"}`)); err != nil {
		t.Fatal(err)
	}

	bodies := make(chan string, 4)
	go func() {
		for {
			kind, conn, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindHTTP {
				conn.Close()
				continue
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				conn.Close()
				return
			}
			body, _ := io.ReadAll(req.Body)
			bodies <- string(body)
			_, _ = io.WriteString(conn, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
			conn.Close()
		}
	}()

	NewTunnelDelivery(st, router).DrainFor(context.Background(), base)

	select {
	case got := <-bodies:
		if got != `{"after":"new"}` {
			t.Fatalf("replayed %s, want the newer commit", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no replay arrived")
	}
	select {
	case extra := <-bodies:
		t.Fatalf("a second replay arrived: %s", extra)
	case <-time.After(300 * time.Millisecond):
	}

	left, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d events still parked after drain", len(left))
	}
}

// TestDrainForBailsWhileOffline pins the bail at the top of DrainFor: it must
// never reach the store while the agent has no live session. DrainEvents is
// destructive — delete then re-insert — so a drain-and-re-park round trip
// necessarily changes the parked row's rowid; an unchanged rowid after
// DrainFor is therefore proof the bail fired and the store was never
// touched. A decoy row parked for a second agent AFTER base's, and never
// touched by DrainFor(base), pins the comparison: SQLite assigns a deleted
// row's replacement the table's current max rowid plus one, so a decoy with
// a higher rowid than base's original guarantees any drain-and-re-park
// round trip lands base's row on a strictly larger rowid than before (parking
// the decoy first would let the reinsert innocuously reclaim base's exact
// original number and mask the very mutation this test exists to catch).
func TestDrainForBailsWhileOffline(t *testing.T) {
	st := openTestStore(t)
	_, base := enrolledAgent(t, st, "1001", "alice")
	_, other := enrolledAgent(t, st, "1002", "bob")
	router := NewRouter() // no session registered: base is offline

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(other, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	before := pendingRowID(t, st, base, "blog", "main")

	NewTunnelDelivery(st, router).DrainFor(context.Background(), base)

	after := pendingRowID(t, st, base, "blog", "main")
	if before != after {
		t.Fatalf("rowid changed %d -> %d: DrainFor touched the store while offline", before, after)
	}
}

func pendingRowID(t *testing.T, st *Store, agentName, app, ref string) int64 {
	t.Helper()
	var id int64
	if err := st.db.QueryRow(
		`SELECT rowid FROM pending_events WHERE agent_name=? AND app=? AND ref=?`,
		agentName, app, ref).Scan(&id); err != nil {
		t.Fatalf("query rowid: %v", err)
	}
	return id
}

// TestDrainForReparksFailedReplay proves a replay that fails is re-parked,
// not dropped: GitHub already got its 202 for the original delivery, so a
// silently lost event here would never be retried by anyone.
func TestDrainForReparksFailedReplay(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
		t.Fatal(err)
	}

	go func() {
		kind, conn, err := sess.AcceptKind()
		if err != nil || kind != tunnel.KindHTTP {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = io.WriteString(conn, "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n")
	}()

	NewTunnelDelivery(st, router).DrainFor(context.Background(), base)

	got, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events after failed replay, want 1 (re-parked, not dropped)", len(got))
	}
	if string(got[0].Payload) != `{"after":"x"}` {
		t.Fatalf("payload = %s, want the original", got[0].Payload)
	}
}
