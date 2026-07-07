package relay

import (
	"context"
	"testing"
)

func TestFakeVerifierStartPollApprove(t *testing.T) {
	f := NewFakeVerifier()
	ctx := context.Background()

	handle, d, err := f.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle == "" || d.UserCode == "" || d.VerificationURI == "" {
		t.Fatalf("empty device auth: %q %+v", handle, d)
	}

	if _, err := f.Poll(ctx, handle); err != ErrAuthPending {
		t.Fatalf("pre-approval Poll err = %v, want ErrAuthPending", err)
	}

	f.Approve(handle, Identity{Subject: "sub-1", Email: "heidi@x.com"})
	id, err := f.Poll(ctx, handle)
	if err != nil {
		t.Fatalf("post-approval Poll: %v", err)
	}
	if id.Subject != "sub-1" || id.Email != "heidi@x.com" {
		t.Fatalf("identity = %+v", id)
	}

	if _, err := f.Poll(ctx, "unknown-handle"); err == nil {
		t.Fatal("Poll(unknown) succeeded, want error")
	}
}
