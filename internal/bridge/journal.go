package bridge

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// pendingTurn is everything needed to deliver an answer to the conversation that asked for it, after
// the process that started the watch is gone. It is the origin record: without it a bridge that
// restarts mid-turn knows a turn exists but not whose message it answers, which is how a finished
// answer ends up unread and a "working…" glyph stays up forever.
type pendingTurn struct {
	PromptID string    `json:"prompt_id"`
	TurnID   string    `json:"turn_id"`
	Space    string    `json:"space"`
	Thread   string    `json:"thread,omitempty"`
	ReplyID  string    `json:"reply_id,omitempty"`
	StatusID string    `json:"status_id,omitempty"`
	Status   string    `json:"status_name,omitempty"` // the Chat message name holding the glyph
	User     string    `json:"user,omitempty"`
	Asked    string    `json:"asked,omitempty"`
	Start    time.Time `json:"started"`
	// Expires keeps the journal bounded. An answer nobody produced is not owed forever; past this the
	// record is dropped on the next load rather than recovered.
	Expires time.Time `json:"expires"`
}

func (p *pendingTurn) watch() *watch {
	return &watch{
		promptID: p.PromptID,
		turnID:   p.TurnID,
		space:    p.Space,
		thread:   p.Thread,
		replyID:  p.ReplyID,
		statusID: p.StatusID,
		bubble:   p.Status,
	}
}

// journal is a small atomic file of owed turns. It is written on state changes only - a watch start,
// a status bubble landing, an answer delivery - so it costs nothing per poll.
//
// It is deliberately not a database and holds no message text beyond a truncated echo of the question,
// which exists so a recovery log line says what was answered. Room content stays out of it.
type journal struct {
	path string
	log  *slog.Logger

	mu    sync.Mutex
	turns map[string]*pendingTurn
	// decisionsWarned keeps a broken evidence file to a single warning; see decisions.go.
	decisionsWarned bool
	// answered is a small in-memory ring of turns that have been delivered, newest last. It exists only
	// so a person quoting a notice back at us resolves to the question that notice was about; the file
	// does not carry it, because that answer is already in Chat.
	answered []*pendingTurn
}

// maxAnsweredRing bounds the recall ring. Anything older is a conversation the bridge should re-read
// rather than reconstruct.
const maxAnsweredRing = 32

// maxJournalTurns bounds the file. Past it the oldest owed records are dropped, because a bridge that
// cannot deliver is better off saying nothing than recovering a backlog of stale answers.
const maxJournalTurns = 50

// openJournal loads path if it exists. An empty path disables persistence and returns a journal that
// keeps records in memory only, so tests and dry runs need no writable volume.
func openJournal(path string, log *slog.Logger) *journal {
	if log == nil {
		log = slog.Default()
	}
	j := &journal{path: path, log: log, turns: map[string]*pendingTurn{}}
	if path == "" {
		return j
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("turn journal unreadable; starting empty", "path", path, "err", err)
		}
		return j
	}
	var loaded []*pendingTurn
	if err := json.Unmarshal(raw, &loaded); err != nil {
		log.Warn("turn journal malformed; starting empty", "path", path, "err", err)
		return j
	}
	now := time.Now()
	for _, p := range loaded {
		if p == nil || p.PromptID == "" || p.TurnID == "" {
			continue
		}
		if !p.Expires.IsZero() && now.After(p.Expires) {
			continue
		}
		j.turns[p.PromptID] = p
	}
	if len(j.turns) > 0 {
		log.Info("turn journal loaded", "path", path, "owed", len(j.turns))
	}
	return j
}

func (j *journal) save(p *pendingTurn) {
	if j == nil || p == nil || p.PromptID == "" {
		return
	}
	j.mu.Lock()
	j.turns[p.PromptID] = p
	if len(j.turns) > maxJournalTurns {
		byAge := make([]*pendingTurn, 0, len(j.turns))
		for _, v := range j.turns {
			byAge = append(byAge, v)
		}
		sort.Slice(byAge, func(i, int2 int) bool { return byAge[i].Start.Before(byAge[int2].Start) })
		for _, v := range byAge[:len(byAge)-maxJournalTurns] {
			delete(j.turns, v.PromptID)
		}
	}
	list := make([]*pendingTurn, 0, len(j.turns))
	for _, v := range j.turns {
		list = append(list, v)
	}
	j.mu.Unlock()
	j.write(list)
}

// update mutates one record in place; missing records are ignored because the answer already landed.
func (j *journal) update(promptID string, fn func(*pendingTurn)) {
	if j == nil || promptID == "" {
		return
	}
	j.mu.Lock()
	p, ok := j.turns[promptID]
	if ok {
		fn(p)
	}
	list := make([]*pendingTurn, 0, len(j.turns))
	for _, v := range j.turns {
		list = append(list, v)
	}
	j.mu.Unlock()
	if ok {
		j.write(list)
	}
}

// remember files a turn in the answered ring without ever making it a debt. Agent-initiated outbound
// posts use it: nothing was owed, but a person quoting the notice back should still resolve to what it
// was about rather than being told the id is unknown.
func (j *journal) remember(p *pendingTurn) {
	if j == nil || p == nil || p.PromptID == "" {
		return
	}
	j.mu.Lock()
	j.answered = append(j.answered, p)
	if len(j.answered) > maxAnsweredRing {
		j.answered = j.answered[len(j.answered)-maxAnsweredRing:]
	}
	j.mu.Unlock()
}

// drop marks an answer delivered (or unowable). Missing keys are fine.
func (j *journal) drop(promptID string) {
	if j == nil || promptID == "" {
		return
	}
	j.mu.Lock()
	p, existed := j.turns[promptID]
	if existed {
		j.answered = append(j.answered, p)
		if len(j.answered) > maxAnsweredRing {
			j.answered = j.answered[len(j.answered)-maxAnsweredRing:]
		}
	}
	delete(j.turns, promptID)
	list := make([]*pendingTurn, 0, len(j.turns))
	for _, v := range j.turns {
		list = append(list, v)
	}
	j.mu.Unlock()
	if existed {
		j.write(list)
	}
}

func (j *journal) pending() []*pendingTurn {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	list := make([]*pendingTurn, 0, len(j.turns))
	for _, v := range j.turns {
		list = append(list, v)
	}
	sort.Slice(list, func(i, int2 int) bool { return list[i].Start.Before(list[int2].Start) })
	return list
}

// recall resolves a turn-id fragment - the part inside a ⟦pi-chat …⟧ marker - to the turn it names,
// owing records first and then recently answered ones.
func (j *journal) recall(ref string) *pendingTurn {
	if j == nil || ref == "" {
		return nil
	}
	ref = strings.ToLower(ref)
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, p := range j.turns {
		if strings.HasPrefix(strings.ToLower(p.TurnID), ref) {
			return p
		}
	}
	for i := len(j.answered) - 1; i >= 0; i-- {
		p := j.answered[i]
		if strings.HasPrefix(strings.ToLower(p.TurnID), ref) {
			return p
		}
	}
	return nil
}

func (j *journal) write(list []*pendingTurn) {
	if j.path == "" {
		return
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		j.log.Warn("turn journal encode failed", "err", err)
		return
	}
	dir := filepath.Dir(j.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		j.log.Warn("turn journal directory unavailable", "path", dir, "err", err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".turns-*.json")
	if err != nil {
		j.log.Warn("turn journal write failed", "path", j.path, "err", err)
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		j.log.Warn("turn journal write failed", "path", name, "err", err)
		return
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		j.log.Warn("turn journal sync failed", "path", name, "err", err)
		return
	}
	if err := tmp.Close(); err != nil {
		j.log.Warn("turn journal close failed", "path", name, "err", err)
		return
	}
	if err := os.Rename(name, j.path); err != nil {
		j.log.Warn("turn journal rename failed", "path", j.path, "err", err)
		return
	}
}
