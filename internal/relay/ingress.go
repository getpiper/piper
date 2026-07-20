package relay

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
)

// maxWebhookBody mirrors the agent-side cap in internal/webhook.
const maxWebhookBody = 5 << 20

// Deliverer hands a verified, routed webhook to one bound agent.
type Deliverer interface {
	Deliver(ctx context.Context, b Binding, eventType string, payload []byte) error
}

// DeliverFunc adapts a function to Deliverer.
type DeliverFunc func(ctx context.Context, b Binding, eventType string, payload []byte) error

func (f DeliverFunc) Deliver(ctx context.Context, b Binding, eventType string, payload []byte) error {
	return f(ctx, b, eventType, payload)
}

// ghEnvelope is the slice of a GitHub webhook the relay needs to route. Payload
// interpretation stays on the box: the relay reads only the routing keys.
type ghEnvelope struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Type  string `json:"type"`
			Login string `json:"login"`
		} `json:"account"`
	} `json:"installation"`
	Sender struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"sender"`
}

// NewGitHubIngress serves the App's single webhook URL. It verifies the App
// signature, keeps installation linkage current, and routes everything else to
// the bound agents of the installation's account. It never routes an event
// whose installation is not linked to an account.
func NewGitHubIngress(st *Store, app *GitHubApp, d Deliverer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if !app.VerifySignature(r.Header.Get("X-Hub-Signature-256"), body) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}

		event := r.Header.Get("X-GitHub-Event")
		if event == "ping" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "pong")
			return
		}

		var env ghEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		installationID := strconv.FormatInt(env.Installation.ID, 10)

		if event == "installation" {
			handleInstallationEvent(st, env, installationID)
			w.WriteHeader(http.StatusAccepted)
			return
		}

		accountID, err := st.AccountForInstallation(installationID)
		if err != nil {
			// Unlinked installation: acknowledge so GitHub stops retrying, but
			// never route. This is the tenancy boundary.
			log.Printf("relay: %s event for unlinked installation %s", event, installationID)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		bindings, err := st.BindingsForRepo(accountID, env.Repository.FullName)
		if err != nil {
			http.Error(w, "routing error", http.StatusInternalServerError)
			return
		}

		// Routing is by repository only. Whether the branch matches is the
		// agent's decision, exactly as in BYO mode; two components filtering the
		// same condition is how pushes end up deploying nowhere.
		w.WriteHeader(http.StatusAccepted)
		for _, b := range bindings {
			go func(b Binding) {
				if err := d.Deliver(context.Background(), b, event, body); err != nil {
					log.Printf("relay: deliver %s to %s/%s: %v", event, b.AgentName, b.App, err)
				}
			}(b)
		}
	})
}

// handleInstallationEvent keeps github_installations in step with GitHub. It is
// written to be order-independent: the OAuth redirect and this webhook race.
func handleInstallationEvent(st *Store, env ghEnvelope, installationID string) {
	switch env.Action {
	case "created", "new_permissions_accepted", "unsuspend":
		senderID := strconv.FormatInt(env.Sender.ID, 10)
		typ := "user"
		if env.Installation.Account.Type == "Organization" {
			typ = "org"
		}
		if err := st.LinkInstallation(installationID, senderID, typ, env.Installation.Account.Login); err != nil {
			log.Printf("relay: link installation %s: %v", installationID, err)
		}
	case "deleted", "suspend":
		if err := st.UnlinkInstallation(installationID); err != nil {
			log.Printf("relay: unlink installation %s: %v", installationID, err)
		}
	}
}
