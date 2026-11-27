package bridge

// A gate decision is evidence, and evidence that lives only in stdout is worth nothing: the pod that
// holds it is deleted by the next rollout, there is no log shipping (Ed, 2026-11-26: "i don't wanna ship
// logs anywhere"), and on 2026-11-26 I reported verdicts I could not substantiate because the window I
// was quoting had already been destroyed with its pod.
//
// So the verdict goes next to the turn journal, on the same persistent volume Ed approved for the state
// file. One JSON line per judgement, appended, rotated at 1 MiB.
//
// What is NOT here is participant text. The line records that the gate was asked, what it decided, how
// long it took and how much context it used - enough to answer "did the bot suppress that, and would it
// again" - and enough to be safe to leave on disk. The prompt that was judged is in the log for a human
// reading it live; duplicating other people's sentences onto a volume is a different risk than printing
// them to a terminal someone owns.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// decisionsFileName is the sibling of the turn journal, in the journal's directory.
const decisionsFileName = "gate-decisions.jsonl"

// maxDecisionFile rotates the record so a long-lived pod cannot grow it without bound. The newest lines
// are the ones a review needs, and rotation keeps them.
const maxDecisionFile = 1 << 20

// gateDecision is one reply-gate judgement. Field names match the log line so a reviewer can move between
// the two without a translation table.
type gateDecision struct {
	TS           time.Time `json:"ts"`
	Flow         string    `json:"flow"`
	Action       string    `json:"action"`
	Mode         string    `json:"mode"`
	Space        string    `json:"space,omitempty"`
	Thread       string    `json:"thread,omitempty"`
	PromptID     string    `json:"prompt,omitempty"`
	ContextChars int       `json:"context_chars"`
	LatencyMS    int64     `json:"latency_ms"`
	ThreadHasBot bool      `json:"thread_has_bot,omitempty"`

	// Kind separates a turn-lifecycle record from a reply-gate judgement. Gate rows leave it empty, so
	// every reader written before 2026-11-27 keeps working unchanged; a reviewer filters on kind=="turn".
	//
	// These rows exist because "feels slow" has two causes and only one of them is the model. Pi-ed's
	// 2026-11-27 read of 50 settled turns measured queueing (a prompt waiting for a session to start on
	// it) as the real term and first-token latency as microseconds, but that evidence lives in the gateway
	// and dies with the pod. queued_ms and run_ms here are the same split from the bridge, which is the
	// only place that sees both: it dispatches the prompt and it watches for the answer.
	Kind     string `json:"kind,omitempty"`
	QueuedMS int64  `json:"queued_ms,omitempty"`
	RunMS    int64  `json:"run_ms,omitempty"`
}

// recordDecision appends one line. Best-effort on purpose: this is evidence, and a full or read-only
// volume must not cost a person their answer. The first failure is logged, then quiet, because a broken
// append inside the reply path would otherwise spam the log it is meant to complement.
func (j *journal) recordDecision(d gateDecision) {
	if j == nil {
		return
	}
	// No journal path means no volume, and "somewhere convenient" is the directory the process happened
	// to start in. Every bridge test runs that way, and before this guard each run left a
	// gate-decisions.jsonl inside internal/bridge/ - which then got committed as though it were evidence.
	if j.path == "" {
		return
	}
	if d.TS.IsZero() {
		d.TS = time.Now().UTC()
	}
	line, err := json.Marshal(d)
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	path := filepath.Join(filepath.Dir(j.path), decisionsFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		if st, serr := f.Stat(); serr == nil && st.Size() > maxDecisionFile {
			// Keep one previous file rather than an unbounded series; the reviewer wants "recently".
			_ = f.Close()
			_ = os.Rename(path, path+".1")
			f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		}
	}
	if err == nil {
		_, err = f.Write(append(line, '\n'))
		_ = f.Close()
	}
	if err != nil && !j.decisionsWarned {
		j.decisionsWarned = true
		j.log.Warn("gate decision record is not being written", "path", path, "err", err)
	}
}
