package chatevents

// A guarded probe against the real Workspace Events API, because the bug this repo actually hit in the
// cluster was a wrong *default constant* that no fake server could catch: the shipped base already
// contained "/subscriptions", so create POSTed to /v1/subscriptions/subscriptions and Google's front
// door answered with an HTML 404, surfacing only as "decode create response: invalid character '<'".
//
// It is opt-in and never runs in `go test ./...`:
//
//	PI_CHAT_REAL_PROBE=1 PI_CHAT_OAUTH_CLIENT_FILE=/root/oauth-client.json \
//	PI_CHAT_OAUTH_TOKEN_FILE=/root/chat-user-token.json \
//	go test ./internal/chatevents -run LiveSubscriptionRenewal -v
//
// Read-only mode (the default) only POSTs a create and expects the 409 that names the subscription
// which already exists for this target: that round trip proves the URL shape, the credential and the
// JSON contract without touching the live subscription. PI_CHAT_REAL_PROBE=renew additionally deletes
// and recreates it, which is what you want when a pod is deaf and you are proving the fix end to end.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveSubscriptionRenewalProbe(t *testing.T) {
	mode := strings.TrimSpace(os.Getenv("PI_CHAT_REAL_PROBE"))
	if mode == "" {
		t.Skip("set PI_CHAT_REAL_PROBE=1 (create-only, expects the 409) or =renew (delete and recreate)")
	}
	cred, err := LoadCredential(
		envOr("PI_CHAT_OAUTH_CLIENT_FILE", "/root/oauth-client.json"),
		envOr("PI_CHAT_OAUTH_SECRET_FILE", ""),
		envOr("PI_CHAT_OAUTH_TOKEN_FILE", "/root/chat-user-token.json"),
	)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	r := NewRenewer(cred,
		envOr("PI_CHAT_USER_PROJECT", "home-ops-513116"),
		envOr("PI_CHAT_USER_TOPIC", "pi-chat-user-events"),
		3*time.Hour,
		slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if !strings.EqualFold(mode, "renew") {
		// Create only. The subscription this deployment uses already exists, so the expected outcome is
		// the duplicate error whose metadata names it. Anything else means the request never reached the
		// API in the shape the API expects.
		_, err := r.create(ctx, mustToken(t, ctx, r))
		var dup *duplicateSubscriptionError
		if errors.As(err, &dup) {
			t.Logf("live API answered the duplicate 409 and named %q: URL shape, credential and JSON contract confirmed", dup.name)
			return
		}
		if err == nil {
			t.Fatalf("create SUCCEEDED, which means a subscription was just created by this probe; delete it or run with PI_CHAT_REAL_PROBE=renew to manage it here")
		}
		t.Fatalf("create did not reach the API as expected: %v", err)
	}

	expire, err := r.Renew(ctx)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	t.Logf("renewed live subscription %q, expireTime=%s", r.name, expire)
	when, err := time.Parse(time.RFC3339, expire)
	if err != nil {
		t.Logf("expireTime was not RFC3339: %q", expire)
		return
	}
	lifetime := time.Until(when)
	t.Logf("measured lifetime from now: %s", lifetime.Round(time.Second))
	if lifetime < 90*time.Minute {
		t.Errorf("a renewed subscription should live ~4h; got %s - the renewal interval (%s) no longer fits inside it", lifetime, r.Interval)
	}
}

func mustToken(t *testing.T, ctx context.Context, r *Renewer) string {
	t.Helper()
	token, err := r.accessToken(ctx)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
