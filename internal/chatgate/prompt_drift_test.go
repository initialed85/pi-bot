package chatgate

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// This is intentionally an integration test: a prompt change in pi-bot must force a deliberate
// re-vendor and a fresh eval rather than silently changing the bridge's measured behaviour. Set
// PI_CHAT_GATE_PROMPT_DRIFT_WARN=1 only for an intentionally offline/flaky CI environment.
func TestVendoredPromptsMatchPiBotMaster(t *testing.T) {
	const rawBase = "https://raw.githubusercontent.com/initialed85/pi-bot/master/eval/chat-gate/prompts/"
	client := &http.Client{Timeout: 10 * time.Second}
	for _, flow := range []string{FlowShouldRespond, FlowRoomRespond, FlowThreadRequest, FlowSummary} {
		want, err := PromptText(flow)
		if err != nil {
			t.Fatal(err)
		}
		// The bridge runs the embedded copy, while the build repo vendors the published one under
		// eval/chat-gate/prompts. Those two must agree inside this repo before anything else is checked.
		canonical, readLocalErr := os.ReadFile("../../eval/chat-gate/prompts/" + flow + ".md")
		if readLocalErr != nil {
			t.Fatalf("published prompt %s is missing from eval/chat-gate/prompts: %v", flow, readLocalErr)
		}
		if string(canonical) != want {
			t.Fatalf("embedded prompt %s differs from the published eval copy; re-vendor or re-run the eval", flow)
		}
		res, err := client.Get(rawBase + flow + ".md")
		if err != nil {
			if os.Getenv("PI_CHAT_GATE_PROMPT_DRIFT_WARN") == "1" {
				t.Logf("prompt drift check skipped for %s: %v", flow, err)
				continue
			}
			t.Fatalf("prompt drift check could not fetch %s (%s): %v; either re-vendor or set PI_CHAT_GATE_PROMPT_DRIFT_WARN=1 only for offline CI", flow, VendoredPromptSource, err)
		}
		body, readErr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if readErr != nil {
			t.Fatalf("read upstream %s: %v", flow, readErr)
		}
		if res.StatusCode == http.StatusNotFound && string(canonical) == want {
			// A prompt that exists in both local copies but not yet on master is a change that has not
			// been pushed, not a silent divergence. Everything else is a hard failure.
			t.Logf("prompt %s not on master yet; local copies agree", flow)
			continue
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("fetch upstream %s: HTTP %s", flow, res.Status)
		}
		if string(body) != want {
			t.Fatalf("vendored prompt %s differs from pi-bot master (%s); either re-vendor or re-run the eval", flow, VendoredPromptSource)
		}
	}
	if !strings.Contains(VendoredPromptSource, "c34f21a") {
		t.Fatalf("vendored prompt source must record the evaluated pi-bot revision, got %q", VendoredPromptSource)
	}
}

func ExampleVendoredPromptSource() {
	fmt.Println(VendoredPromptSource)
	// Output: initialed85/pi-bot@c34f21af50966d310b3427531f093f5f7afa4c81
}
