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

	"github.com/getpiper/piper/internal/tunnel"
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
