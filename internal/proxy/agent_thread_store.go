package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Persistent bindings between a client conversation and a Notion Agent thread.
//
// A client conversation is identified by the account/workspace, the selected
// model, an optional client session key and the content of the messages already
// sent to the thread (a hash chain; no message text is stored). A request may
// continue a thread only when its message list starts with exactly those
// messages, followed by the assistant reply the client echoes back and then
// new user/tool messages. Anything else (edit, rollback, compaction, another
// account or session, concurrent fork) starts a new thread with the full
// history instead of writing into an unrelated thread.

const (
	agentStateSubmitting = "submitting" // intent recorded, upstream acknowledgement unknown
	agentStateSubmitted  = "submitted"  // upstream accepted the message, reply pending
	agentStateCompleted  = "completed"  // reply read; LastSeq marks its end
	agentStateFailed     = "failed"     // first turn of a bound thread ended in failure; retry stays in the thread

	agentModeNew      = "new"
	agentModeFollowUp = "followup"
	agentModeReplay   = "replay"

	agentThreadRecordTTL = 14 * 24 * time.Hour
	agentThreadRecordCap = 500
)

type agentThreadRecord struct {
	ClientInterrupted bool `json:"client_interrupted,omitempty"`
	// ResendContext: Notion compacted this thread's context on its own; the next
	// follow-up re-sends the project context and local action protocol, which
	// the server-side summary may have dropped. NotionCompactions counts them.
	ResendContext     bool               `json:"resend_context,omitempty"`
	NotionCompactions int                `json:"notion_compactions,omitempty"`
	EditPrefixHash    string             `json:"edit_prefix_hash,omitempty"`
	EditPrefixLen     int                `json:"edit_prefix_len,omitempty"`
	EditAttachStart   int                `json:"edit_attach_start,omitempty"`
	Correction        bool               `json:"correction,omitempty"`
	CorrectionCount   int                `json:"correction_count,omitempty"`
	ClientKey         string             `json:"client_key,omitempty"`
	AccountID         string             `json:"account_id"`
	SpaceID           string             `json:"space_id"`
	Model             string             `json:"model,omitempty"`
	ThreadID          string             `json:"thread_id"`
	PrefixLen         int                `json:"prefix_len"`
	PrefixHash        string             `json:"prefix_hash"`
	ConvLen           int                `json:"conv_len"`
	ConvHash          string             `json:"conv_hash"`
	Attachments       int                `json:"attachments"`
	TurnStart         int                `json:"turn_start"`
	TurnAttach        int                `json:"turn_attach"`
	PendingFollowUp   bool               `json:"pending_follow_up"`
	ClientMessageID   string             `json:"client_message_id"`
	State             string             `json:"state"`
	Baseline          agentSeq           `json:"baseline"`
	LastSeq           agentSeq           `json:"last_seq"`
	Turns             int                `json:"turns"`
	RootHash          string             `json:"root_hash,omitempty"`   // first non-system message; identifies the conversation
	SystemHash        string             `json:"system_hash,omitempty"` // last system/tool context sent to the thread
	Tooled            bool               `json:"tooled,omitempty"`
	TurnSysUpdate     bool               `json:"turn_sys_update,omitempty"`
	TurnCompacted     bool               `json:"turn_compacted,omitempty"` // this turn re-based the binding on a client compaction summary (agent_compaction.go)
	Compactions       int                `json:"compactions,omitempty"`
	AuxSummaryHash    string             `json:"aux_summary_hash,omitempty"` // hash of the summary a Claude /compact summarisation side request produced; no text
	AuxConvHash       string             `json:"aux_conv_hash,omitempty"`    // conversation state that side request belonged to
	AuxRewound        bool               `json:"aux_rewound,omitempty"`      // summarisation request dropped the bound thread's last assistant reply
	AuxCount          int                `json:"aux_count,omitempty"`
	AuxFailures          int                `json:"aux_failures,omitempty"`
	Failures             int                `json:"failures,omitempty"`
	TurnUserNormLen      int                `json:"turn_user_norm_len,omitempty"`
	CommittedConvLen     int                `json:"committed_conv_len,omitempty"`
	CommittedConvHash    string             `json:"committed_conv_hash,omitempty"`
	CommittedUserNormLen int                `json:"committed_user_norm_len,omitempty"`
	Prev                 *agentThreadRecord `json:"prev,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
	UpdatedAt            time.Time          `json:"updated_at"`
}

type agentPlanInput struct {
	ClientKey   string
	SubagentID  string
	AccountID   string
	SpaceID     string
	Model       string
	Messages    []ChatMessage
	Attachments []UploadedAttachment
	Tooled      bool // request carries client tools (main conversation, not a helper call)
	Recap       bool
}

// agentTurn describes how one request talks to Notion.
type agentTurn struct {
	correction *agentPlanInput
	// supersede: the new request continues the last stable state while the bound
	// turn was sent but never confirmed (5xx / lost connection). The transcript is
	// probed first; only a turn upstream never accepted is treated as not sent.
	supersede       *agentPlanInput
	store           *agentThreadStore
	rec             *agentThreadRecord
	mode            string
	threadID        string
	title           string
	keyed           bool // bound to a stable client session (no silent new threads)
	prev            *agentThreadRecord
	followUp        bool // send to an existing thread instead of creating one
	submit          bool // false when an earlier attempt already sent this turn
	probe           bool // earlier attempt may or may not have been accepted
	baseline        agentSeq
	prompt          string
	attachments     []UploadedAttachment
	offloads        []FileAttachment // full text of shortened outputs, attached as files (agent_size.go)
	offloadFallback string           // prompt with plain truncation, used when the offload upload fails
	unfitPrompt     string           // prompt before the size guard (fitTurnPrompt may run once per attempt)
	fittedPrompt    string           // what the last fitTurnPrompt produced
	clientMessageID string
	unlock          func()
	sideReply       func(text string) // one-shot side request: called with the finished reply (agent_compaction.go)
	sideFail        func()            // one-shot side request: called when its turn failed
}

func newUntrackedAgentTurn(in agentPlanInput) *agentTurn {
	prompt := agentThreadPrompt(in.Messages)
	if isCompactSummarisationRequest(in.Messages) {
		prompt = agentThreadSummaryPrompt(in.Messages)
	}
	return &agentTurn{
		mode: agentModeNew, threadID: generateUUIDv4(), title: agentTurnTitle(in), clientMessageID: generateUUIDv4(), submit: true,
		prompt: prompt, attachments: in.Attachments,
	}
}

func (t *agentTurn) release() {
	if t != nil && t.unlock != nil {
		t.unlock()
		t.unlock = nil
	}
}
func (t *agentTurn) markSubmitted() {
	if t.store != nil && t.rec != nil {
		t.store.setState(t.rec, agentStateSubmitted, 0)
	}
}
func (t *agentTurn) complete(maxSeq agentSeq) {
	if t.store != nil && t.rec != nil {
		t.store.setState(t.rec, agentStateCompleted, maxSeq)
	}
}
func (t *agentTurn) markNotionCompacted() {
	if t.store == nil || t.rec == nil || !t.keyed {
		return
	}
	t.store.mu.Lock()
	if t.store.records[t.threadID] == t.rec {
		t.rec.ResendContext = true
		t.rec.NotionCompactions++
		t.store.saveLocked()
		log.Printf("[agent-thread] Notion compacted the context of thread %s on its own (server-side compaction #%d); the next follow-up re-sends the project context and local action protocol", shortID(t.threadID), t.rec.NotionCompactions)
	}
	t.store.mu.Unlock()
}

func (t *agentTurn) needsResendContext() bool {
	if t == nil || t.store == nil || t.rec == nil || !t.keyed {
		return false
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	return t.store.records[t.threadID] == t.rec && t.rec.ResendContext
}

func (t *agentTurn) consumeResendContext() {
	if t == nil || t.store == nil || t.rec == nil || !t.keyed {
		return
	}
	t.store.mu.Lock()
	if t.store.records[t.threadID] == t.rec && t.rec.ResendContext {
		t.rec.ResendContext = false
		t.store.saveLocked()
	}
	t.store.mu.Unlock()
}

func (t *agentTurn) advanceBaseline(maxSeq agentSeq) {
	if t == nil || maxSeq <= t.baseline {
		return
	}
	t.baseline = maxSeq
	if t.store == nil || t.rec == nil {
		return
	}
	t.store.mu.Lock()
	if t.store.records[t.threadID] == t.rec {
		if maxSeq > t.rec.Baseline {
			t.rec.Baseline = maxSeq
		}
		if maxSeq > t.rec.LastSeq {
			t.rec.LastSeq = maxSeq
		}
		t.rec.UpdatedAt = time.Now()
		t.store.saveLocked()
	}
	t.store.mu.Unlock()
}

func (t *agentTurn) observeReply(text string) {
	if t.sideReply != nil {
		t.sideReply(text)
	}
}
func (t *agentTurn) discard() {
	if t.store != nil && t.rec != nil {
		t.store.discard(t.rec)
	}
}

// followUpModel returns notionModel only when this follow-up switched models
// relative to the bound thread's previous turn, avoiding duplicate
// provider_handoff status entities on unchanged turns (#68, #89).
func (t *agentTurn) followUpModel(notionModel string) string {
	if t == nil || notionModel == "" {
		return ""
	}
	var prevModel string
	if t.prev != nil {
		prevModel = t.prev.Model
	} else if t.rec != nil {
		prevModel = t.rec.Model
	}
	if prevModel != "" && prevModel != notionModel {
		return notionModel
	}
	return ""
}

type agentThreadStore struct {
	mu      sync.Mutex
	path    string
	records map[string]*agentThreadRecord
	locks   map[string]*sync.Mutex
}

func newAgentThreadStore(path string) *agentThreadStore {
	s := &agentThreadStore{path: path, records: map[string]*agentThreadRecord{}, locks: map[string]*sync.Mutex{}}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[agent-thread] cannot read bindings: %v (starting empty)", err)
		}
		return s
	}
	var list []*agentThreadRecord
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("[agent-thread] ignoring unreadable bindings file: %v", err)
		return s
	}
	for _, rec := range list {
		if rec != nil && rec.ThreadID != "" {
			s.records[rec.ThreadID] = rec
		}
	}
	s.pruneLocked(time.Now())
	return s
}

var (
	agentStoreOnce sync.Once
	agentStoreInst *agentThreadStore
)

// globalAgentThreadStore returns nil when resume is disabled.
func globalAgentThreadStore() *agentThreadStore {
	if AppConfig == nil || !AppConfig.Proxy.AgentThread || !AppConfig.AgentThreadResumeEnabled() {
		return nil
	}
	agentStoreOnce.Do(func() {
		agentStoreInst = newAgentThreadStore(filepath.Join(AppConfig.Server.AccountsDir, ".agent_threads.state"))
	})
	return agentStoreInst
}

func agentClientKey(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("agent-client:" + sessionKey))
	return hex.EncodeToString(sum[:])[:32]
}

func agentAccountKey(acc *Account) string {
	if acc == nil {
		return ""
	}
	if acc.AccountID != "" {
		return acc.AccountID
	}
	if acc.UserID != "" && acc.SpaceID != "" {
		return ComputeAccountID(acc.UserID, acc.SpaceID)
	}
	return "email:" + acc.UserEmail + "|" + acc.SpaceID
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func uuidFromSeed(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// agentMessageDigest identifies a message for conversation matching. Assistant
// text is deliberately ignored (clients echo it back with cosmetic changes);
// tool-call IDs still tie tool results to the right request.
func agentMessageDigest(m ChatMessage) string {
	h := sha256.New()
	h.Write([]byte(m.Role))
	h.Write([]byte{0})
	switch m.Role {
	case "system":
		h.Write([]byte(normalizeSessionSystemContent(m.Content)))
	case "user":
		h.Write([]byte(normalizeSessionUserContent(m.Content)))
	case "assistant":
		for _, call := range m.ToolCalls {
			h.Write([]byte(call.ID))
			h.Write([]byte{0})
		}
	case "tool":
		h.Write([]byte(m.ToolCallID))
		h.Write([]byte{0})
		h.Write([]byte(m.Name))
		h.Write([]byte{0})
		h.Write([]byte(strings.TrimSpace(m.Content)))
	default:
		h.Write([]byte(strings.TrimSpace(m.Content)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// agentChain returns n+1 hashes; entry i commits to the first i digests.
func agentChain(digests []string) []string {
	chain := make([]string, len(digests)+1)
	sum := sha256.Sum256([]byte("agent-chain-v1"))
	chain[0] = hex.EncodeToString(sum[:])
	for i, digest := range digests {
		next := sha256.Sum256([]byte(chain[i] + "\x00" + digest))
		chain[i+1] = hex.EncodeToString(next[:])
	}
	return chain
}

func agentChains(messages []ChatMessage) (full, conv []string) {
	var all, nonSystem []string
	for _, m := range messages {
		d := agentMessageDigest(m)
		all = append(all, d)
		if m.Role != "system" {
			nonSystem = append(nonSystem, d)
		}
	}
	return agentChain(all), agentChain(nonSystem)
}

// validFollowUp: the message right after the recorded prefix is the assistant
// reply already held by the thread, and only new user/tool messages follow.
func validFollowUp(messages []ChatMessage, prefixLen int) bool {
	if prefixLen <= 0 || prefixLen >= len(messages) || messages[prefixLen].Role != "assistant" {
		return false
	}
	delta := messages[prefixLen+1:]
	if len(delta) == 0 {
		return false
	}
	last := delta[len(delta)-1]
	if last.Role != "user" && last.Role != "tool" {
		return false
	}
	for _, m := range delta {
		if m.Role != "user" && m.Role != "tool" && !isSyntheticResumeAssistant(m) {
			return false
		}
	}
	return true
}

func (s *agentThreadStore) matchLocked(in agentPlanInput, full []string, exclude map[string]bool) *agentThreadRecord {
	n := len(in.Messages)
	var best *agentThreadRecord
	for _, rec := range s.records {
		if exclude[rec.ThreadID] || rec.ClientKey != in.ClientKey || rec.AccountID != in.AccountID || rec.SpaceID != in.SpaceID || rec.Model != in.Model {
			continue
		}
		if rec.PrefixLen <= 0 || rec.PrefixLen > n || full[rec.PrefixLen] != rec.PrefixHash {
			continue
		}
		if rec.PrefixLen == n {
			if rec.Attachments != len(in.Attachments) {
				continue
			}
		} else if rec.State != agentStateCompleted || !validFollowUp(in.Messages, rec.PrefixLen) || len(in.Attachments) < rec.Attachments {
			continue
		}
		if best == nil || rec.PrefixLen > best.PrefixLen || (rec.PrefixLen == best.PrefixLen && rec.UpdatedAt.After(best.UpdatedAt)) {
			best = rec
		}
	}
	return best
}

func (s *agentThreadStore) lockFor(threadID string) *sync.Mutex {
	lk := s.locks[threadID]
	if lk == nil {
		lk = &sync.Mutex{}
		s.locks[threadID] = lk
	}
	return lk
}

// begin finds or creates the binding for a request and returns exclusive
// access to its thread until release is called.
func (s *agentThreadStore) begin(in agentPlanInput) (*agentTurn, error) {
	if isUntrackedHelperRequest(in) {
		return newUntrackedAgentTurn(in), nil
	}
	if key, recap := awayRecapClientKey(in); recap {
		in.ClientKey = key
		in.Recap = true
	}
	if in.ClientKey != "" {
		return s.beginKeyed(in)
	}
	full, conv := agentChains(in.Messages)
	n := len(in.Messages)
	exclude := map[string]bool{}
	for attempt := 0; attempt < 6; attempt++ {
		s.mu.Lock()
		rec := s.matchLocked(in, full, exclude)
		if rec == nil {
			t := s.newTurnLocked(in, full, conv)
			s.mu.Unlock()
			return t, nil
		}
		lk := s.lockFor(rec.ThreadID)
		s.mu.Unlock()
		lk.Lock()
		s.mu.Lock()
		if cur, ok := s.records[rec.ThreadID]; !ok || cur != rec || s.matchLocked(in, full, exclude) != rec {
			s.mu.Unlock()
			lk.Unlock()
			continue
		}
		t := &agentTurn{store: s, rec: rec, threadID: rec.ThreadID, unlock: lk.Unlock, clientMessageID: rec.ClientMessageID}
		if rec.PrefixLen == n {
			t.mode = agentModeReplay
			t.baseline = rec.Baseline
			t.followUp = rec.PendingFollowUp
			if rec.State != agentStateCompleted && rec.Prev != nil {
				prev := *rec.Prev
				t.prev = &prev
			}
			if rec.State == agentStateSubmitting {
				t.submit, t.probe = true, true
				t.prompt, t.attachments = s.promptForLocked(rec, in)
				if !rec.PendingFollowUp {
					t.title = agentTurnTitle(in)
				}
			}
			s.mu.Unlock()
			log.Printf("[agent-thread] same request seen again; reusing thread %s (state=%s) without re-sending", shortID(rec.ThreadID), rec.State)
			return t, nil
		}
		prev := *rec
		prev.Prev = nil
		t.prev = &prev // Pre-submit upload failures must restore unkeyed continuations too.
		rec.Prev = &prev
		delta := in.Messages[rec.PrefixLen+1:]
		rec.Baseline = rec.LastSeq
		rec.TurnStart = rec.PrefixLen + 1
		rec.TurnAttach = rec.Attachments
		rec.PendingFollowUp = true
		rec.PrefixLen, rec.PrefixHash = n, full[n]
		rec.ConvLen, rec.ConvHash = len(conv)-1, conv[len(conv)-1]
		rec.Attachments = len(in.Attachments)
		rec.State = agentStateSubmitting
		rec.ClientMessageID = uuidFromSeed(rec.ThreadID + "|" + full[n])
		rec.UpdatedAt = time.Now()
		s.saveLocked()
		t.mode, t.followUp, t.submit = agentModeFollowUp, true, true
		t.baseline = rec.Baseline
		t.clientMessageID = rec.ClientMessageID
		t.prompt = agentThreadFollowUpPrompt(delta)
		t.attachments = append([]UploadedAttachment(nil), in.Attachments[rec.TurnAttach:]...)
		s.mu.Unlock()
		log.Printf("[agent-thread] continuing thread %s with %d new message(s)", shortID(rec.ThreadID), len(delta))
		return t, nil
	}
	return nil, fmt.Errorf("Notion Agent thread binding is busy; refusing to guess a conversation")
}

func (s *agentThreadStore) promptForLocked(rec *agentThreadRecord, in agentPlanInput) (string, []UploadedAttachment) {
	attach := in.Attachments
	if rec.TurnAttach > 0 && rec.TurnAttach <= len(attach) {
		attach = attach[rec.TurnAttach:]
	}
	if rec.PendingFollowUp && rec.TurnStart > 0 && rec.TurnStart <= len(in.Messages) {
		return agentThreadFollowUpPrompt(in.Messages[rec.TurnStart:]), append([]UploadedAttachment(nil), attach...)
	}
	if isCompactSummarisationRequest(in.Messages) {
		return agentThreadSummaryPrompt(in.Messages), append([]UploadedAttachment(nil), in.Attachments...)
	}
	return agentThreadPrompt(in.Messages), append([]UploadedAttachment(nil), in.Attachments...)
}

func (s *agentThreadStore) newTurnLocked(in agentPlanInput, full, conv []string) *agentTurn {
	n := len(in.Messages)
	now := time.Now()
	rec := &agentThreadRecord{
		ClientKey: in.ClientKey, AccountID: in.AccountID, SpaceID: in.SpaceID, Model: in.Model,
		ThreadID: generateUUIDv4(), PrefixLen: n, PrefixHash: full[n],
		ConvLen: len(conv) - 1, ConvHash: conv[len(conv)-1], Attachments: len(in.Attachments),
		State: agentStateSubmitting, CreatedAt: now, UpdatedAt: now,
	}
	rec.ClientMessageID = uuidFromSeed(rec.ThreadID + "|create")
	s.records[rec.ThreadID] = rec
	lk := s.lockFor(rec.ThreadID)
	lk.Lock()
	s.pruneLocked(now)
	s.saveLocked()
	prompt := agentThreadPrompt(in.Messages)
	if isCompactSummarisationRequest(in.Messages) {
		prompt = agentThreadSummaryPrompt(in.Messages)
	}
	return &agentTurn{
		store: s, rec: rec, mode: agentModeNew, threadID: rec.ThreadID, title: agentTurnTitle(in), submit: true, clientMessageID: rec.ClientMessageID,
		prompt: prompt, attachments: in.Attachments, unlock: lk.Unlock,
	}
}

// restart discards a binding that cannot be used and opens a new thread for the
// same request. The new thread carries the full client history.
func (s *agentThreadStore) restart(old *agentTurn, in agentPlanInput) (*agentTurn, error) {
	if s == nil {
		return nil, fmt.Errorf("Notion Agent cannot restart without a binding store")
	}
	full, conv := agentChains(in.Messages)
	s.mu.Lock()
	if old.rec != nil {
		delete(s.records, old.rec.ThreadID)
		delete(s.locks, old.rec.ThreadID)
	}
	t := s.newTurnLocked(in, full, conv)
	if in.ClientKey != "" {
		t.keyed = true
		s.fillMetaLocked(t.rec, conv, agentSystemHash(in.Messages), in)
		s.saveLocked()
	}
	s.mu.Unlock()
	old.release()
	return t, nil
}

func (s *agentThreadStore) setState(rec *agentThreadRecord, state string, lastSeq agentSeq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[rec.ThreadID]; !ok {
		return
	}
	prev := rec.State
	rec.State = state
	rec.UpdatedAt = time.Now()
	if state == agentStateCompleted {
		if lastSeq > rec.LastSeq {
			rec.LastSeq = lastSeq
		}
		// Replaying an already completed turn must not count it twice.
		if prev != agentStateCompleted {
			rec.Turns++
		}
		// Delivering the reply consumes any earlier Ctrl+C permission. A real new
		// cancellation re-marks the record after this (see markClientInterrupted).
		rec.ClientInterrupted = false
		rec.TurnUserNormLen = 0
		rec.CommittedConvLen = 0
		rec.CommittedConvHash = ""
		rec.CommittedUserNormLen = 0
		rec.Prev = nil
	}
	s.saveLocked()
}

func (s *agentThreadStore) discard(rec *agentThreadRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.records[rec.ThreadID]; ok && cur == rec {
		delete(s.records, rec.ThreadID)
		s.saveLocked()
	}
}

// preferredAccount lets the router resume a persisted thread on the account
// that owns it (for example after a proxy restart).
func (s *agentThreadStore) preferredAccount(messages []ChatMessage, clientKey, model string) string {
	return s.preferredAccountFiltered(messages, clientKey, model, nil)
}

func (s *agentThreadStore) preferredAccountFiltered(messages []ChatMessage, clientKey, model string, usable func(accountID string) bool) string {
	if s == nil {
		return ""
	}
	_, conv := agentChains(messages)
	nonSystem := len(conv) - 1
	var nonSystemMsgs []ChatMessage
	for _, m := range messages {
		if m.Role != "system" {
			nonSystemMsgs = append(nonSystemMsgs, m)
		}
	}
	isCompacted := clientKey != "" && len(nonSystemMsgs) > 0 && (compactionShaped(nonSystemMsgs) || strings.Contains(nonSystemMsgs[0].Content, claudeCompactionPrefix))
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *agentThreadRecord
	for _, rec := range s.records {
		if rec.ClientKey != clientKey || (clientKey == "" && rec.Model != model) || rec.ConvLen <= 0 {
			continue
		}
		if clientKey != "" {
			prefixMatch := rec.ConvLen <= nonSystem && conv[rec.ConvLen] == rec.ConvHash && (rec.ConvLen == nonSystem || validFollowUp(nonSystemMsgs, rec.ConvLen))
			rootMatch := rec.RootHash != "" && nonSystem >= 1 && conv[1] == rec.RootHash
			if !prefixMatch && !rootMatch && !isCompacted {
				continue
			}
		} else {
			if rec.ConvLen > nonSystem || conv[rec.ConvLen] != rec.ConvHash {
				continue
			}
			if rec.ConvLen < nonSystem && !validFollowUp(nonSystemMsgs, rec.ConvLen) {
				continue
			}
		}
		if best == nil || rec.ConvLen > best.ConvLen || (rec.ConvLen == best.ConvLen && rec.UpdatedAt.After(best.UpdatedAt)) {
			best = rec
		}
	}
	if best == nil {
		return ""
	}
	if usable != nil && !usable(best.AccountID) {
		return ""
	}
	return best.AccountID
}

// agentThreadPreferredAccountID returns the account ID owning a resumable
// thread for these client messages, or "" when none is recorded.
func agentThreadPreferredAccountID(messages []ChatMessage, clientSessionKey, model string) string {
	return agentThreadPreferredAccountIDForPool(nil, messages, clientSessionKey, model)
}

func agentThreadPreferredAccountIDForPool(pool *AccountPool, messages []ChatMessage, clientSessionKey, model string) string {
	store := globalAgentThreadStore()
	if store == nil {
		return ""
	}
	notionModel, err := agentThreadModelID(model)
	if err != nil {
		return ""
	}
	var usable func(string) bool
	if pool != nil {
		usable = func(accountID string) bool {
			acc := pool.FindByAccountID(accountID)
			return acc != nil && !pool.isUnusable(acc)
		}
	}
	return store.preferredAccountFiltered(messages, agentClientKey(clientSessionKey), notionModel, usable)
}

func (s *agentThreadStore) pruneLocked(now time.Time) {
	for id, rec := range s.records {
		if now.Sub(rec.UpdatedAt) > agentThreadRecordTTL {
			delete(s.records, id)
			delete(s.locks, id)
		}
	}
	if len(s.records) <= agentThreadRecordCap {
		return
	}
	recs := make([]*agentThreadRecord, 0, len(s.records))
	for _, rec := range s.records {
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].UpdatedAt.After(recs[j].UpdatedAt) })
	for _, rec := range recs[agentThreadRecordCap:] {
		delete(s.records, rec.ThreadID)
		delete(s.locks, rec.ThreadID)
	}
}

// saveLocked persists bindings (hashes and IDs only; no message text or
// credentials). Failure is logged: the in-memory binding keeps working.
func (s *agentThreadStore) saveLocked() {
	if s.path == "" {
		return
	}
	list := make([]*agentThreadRecord, 0, len(s.records))
	for _, rec := range s.records {
		list = append(list, rec)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ThreadID < list[j].ThreadID })
	data, err := json.Marshal(list)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(s.path), 0o700)
	}
	if err == nil {
		tmp := s.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	if err != nil {
		log.Printf("[agent-thread] cannot persist bindings: %v", err)
	}
}
