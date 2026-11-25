package chatevents

// The live probe for the identity lookup. The unit suite proves the parsing and the caching against a
// fake server; it cannot prove that Google answers this read at all, that the query shape is accepted,
// or that the sender object really carries an email. Every one of those was guessed at least once in
// this project and each guess that was wrong looked like a working system.
//
//	go test ./internal/chatevents -run LiveSenderLookup -v
//
// Read-only: one GET of a space's message list, which is the same read the bridge does when it meets a
// user id it has never seen. It is opt-in because it needs Ed's OAuth credential files.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveSenderLookupProbe(t *testing.T) {
	if strings.TrimSpace(os.Getenv("PI_CHAT_REAL_PROBE")) == "" {
		t.Skip("set PI_CHAT_REAL_PROBE=1 to read the real Chat API (read-only: one messages.list)")
	}
	cred, err := LoadCredential(
		envOr("PI_CHAT_OAUTH_CLIENT_FILE", "/root/oauth-client.json"),
		envOr("PI_CHAT_OAUTH_SECRET_FILE", ""),
		envOr("PI_CHAT_OAUTH_TOKEN_FILE", "/root/chat-user-token.json"),
	)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}

	space := envOr("PI_CHAT_LIVE_SPACE", "spaces/AAAA1pNBS1A")
	lookup := NewSenderLookup(cred, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The id the bridge has actually seen on the room path, measured from a production log line.
	for _, userID := range []string{
		envOr("PI_CHAT_LIVE_USER", "users/113070394191408311024"),
		"users/108049318480764095606",
	} {
		identity, found, err := lookup.Resolve(ctx, space, userID)
		if err != nil {
			t.Errorf("resolve %s in %s: %v", userID, space, err)
			continue
		}
		if !found {
			t.Logf("%s in %s: not resolved (that person has no recent message in this space that Chat published an email for)", userID, space)
			continue
		}
		if !strings.Contains(identity.Email, "@") {
			t.Errorf("resolve %s: email %q is not an email", userID, identity.Email)
			continue
		}
		// Masked: this probe is run by whoever is debugging, and a room contains other people's identities.
		t.Logf("%s in %s -> resolved, email=%s display_name=%s (calls so far %d)",
			userID, space, maskEmail(identity.Email), maskName(identity.DisplayName), lookup.Calls())
	}

	if lookup.Calls() == 0 {
		t.Errorf("no Chat read happened; the probe proved nothing")
	}
}

func maskEmail(email string) string {
	name, domain, ok := strings.Cut(email, "@")
	if !ok || name == "" {
		return "***"
	}
	if len(name) == 1 {
		return name + "***@" + domain
	}
	return name[:1] + strings.Repeat("*", len(name)-1) + "@" + domain
}

func maskName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	parts := strings.Fields(name)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		out = append(out, part[:1]+strings.Repeat("*", max(0, len(part)-1)))
	}
	return strings.Join(out, " ")
}
