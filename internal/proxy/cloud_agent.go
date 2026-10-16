package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CloudAgent serves raw personal-Agent tasks independently of the local action
// protocol. One serialized journal owns submission IDs and thread/account pins.
// An uncertain submission is never sent again: reads recover the original thread.
type CloudAgent struct {
	journalLock *os.File
	mu          sync.Mutex
	path        string
	records     map[string]*cloudRecord
	loadErr     error
	pool        *AccountPool
	client      *http.Client
}
type cloudRecord struct {
	RequestID   string   `json:"request_id"`
	Fingerprint string   `json:"fingerprint"`
	AccountID   string   `json:"account_id"`
	SpaceID     string   `json:"space_id"`
	ThreadID    string   `json:"thread_id"`
	MessageID   string   `json:"message_id"`
	Model       string   `json:"model"`
	Effort      string   `json:"effort"`
	State       string   `json:"state"`
	Baseline    agentSeq `json:"baseline"`
	LastSeq     agentSeq `json:"last_seq"`
}
type cloudRequest struct {
	RequestID string `json:"requestId"`
	AccountID string `json:"accountId"`
	ThreadID  string `json:"threadId"`
	Task      string `json:"task"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Title     string `json:"title"`
	Category  string `json:"category"`
}

func NewCloudAgent(pool *AccountPool, accountsDir string) *CloudAgent {
	c := &CloudAgent{path: filepath.Join(accountsDir, ".cloud_agent.state"), records: map[string]*cloudRecord{}, pool: pool, client: agentThreadHTTPClient(30 * time.Second)}
	if err := os.MkdirAll(accountsDir, 0700); err != nil {
		c.loadErr = err
		return c
	}
	lock, err := os.OpenFile(c.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err == nil {
		err = lock.Chmod(0600)
	}
	if err == nil {
		err = lockCloudJournal(lock)
	}
	if err != nil {
		if lock != nil {
			_ = lock.Close()
		}
		c.loadErr = err
		return c
	}
	c.journalLock = lock
	data, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return c
	}
	if err != nil {
		c.loadErr = err
		return c
	}
	c.loadErr = json.Unmarshal(data, &c.records)
	if c.records == nil {
		c.records = map[string]*cloudRecord{}
		c.loadErr = errors.New("invalid cloud journal")
	}
	for id, r := range c.records {
		if r == nil || r.RequestID != id || r.ThreadID == "" || r.AccountID == "" || r.SpaceID == "" {
			c.loadErr = errors.New("invalid cloud journal")
			break
		}
	}
	return c
}

// Close releases journal ownership after the HTTP server has stopped.
func (c *CloudAgent) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadErr = errors.New("closed")
	if c.journalLock != nil {
		err := c.journalLock.Close()
		c.journalLock = nil
		return err
	}
	return nil
}
func (c *CloudAgent) save() error {
	data, err := json.Marshal(c.records)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(c.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), ".cloud-agent-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, c.path)
		if err == nil {
			err = syncCloudJournalDir(filepath.Dir(c.path))
		}
	}
	return err
}
func cloudHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (c *CloudAgent) latest(thread string) *cloudRecord {
	var found *cloudRecord
	for _, r := range c.records {
		if r.ThreadID == thread && (found == nil || r.Baseline > found.Baseline) {
			found = r
		}
	}
	return found
}
func (c *CloudAgent) account(r *cloudRecord) *Account {
	a := c.pool.FindByAccountID(r.AccountID)
	if a == nil || a.SpaceID != r.SpaceID {
		return nil
	}
	return a
}
func (c *CloudAgent) transcript(ctx context.Context, a *Account, r *cloudRecord) (agentTranscriptPage, []json.RawMessage, error) {
	var rawPatches []json.RawMessage
	page, err := collectAgentTranscript(func(cursor string) (agentTranscriptPage, error) {
		var p agentTranscriptPage
		body := map[string]interface{}{"spaceId": r.SpaceID, "threadId": r.ThreadID, "direction": "backward", "limit": agentTranscriptPageLimit}
		if cursor != "" {
			body["cursor"] = cursor
		}
		var raw struct {
			Patches []json.RawMessage `json:"patches"`
		}
		var data json.RawMessage
		err := agentPost(ctx, c.client, a, "getThreadTranscript", body, &data)
		if err == nil {
			err = json.Unmarshal(data, &p)
		}
		if err == nil {
			err = json.Unmarshal(data, &raw)
		}
		if err == nil {
			rawPatches = append(raw.Patches, rawPatches...)
		}
		return p, err
	}, r.Baseline, nil)
	return page, rawPatches, err
}

// A completed request is a fixed turn, even while its thread continues.
func cloudCompletedPage(p agentTranscriptPage, end agentSeq) agentTranscriptPage {
	p.Session.Status = "idle"
	p.Session.Sequence = end
	var patches []agentTranscriptPatch
	for _, patch := range p.Patches {
		if patch.Op == "put" && patch.Entity.Sequence > end {
			continue
		}
		if patch.Op == "committed" && patch.Sequence > end {
			continue
		}
		patches = append(patches, patch)
	}
	p.Patches = patches
	return p
}

// cloudInterrupted requires a complete, idle transcript with an explicit native
// interruption receipt. A failed journal entry alone is never permission to send.
func cloudInterrupted(p agentTranscriptPage, baseline agentSeq) bool {
	if p.Type != agentTranscriptPageType || p.Session.Status != "idle" || p.HasMoreBackward {
		return false
	}
	entities := map[string]agentTranscriptEntity{}
	for _, patch := range p.Patches {
		switch patch.Op {
		case "put":
			entities[patch.Entity.ID] = patch.Entity
		case "remove":
			delete(entities, patch.ID)
		}
	}
	var terminal, activity agentSeq
	interrupted := false
	for _, e := range entities {
		if e.Sequence <= baseline {
			continue
		}
		if e.Kind == "turn_completed" || e.Kind == "turn_failed" || e.Kind == "error" {
			if e.Sequence >= terminal {
				terminal = e.Sequence
				interrupted = e.Kind == "turn_completed" && e.StopReason == "interrupted"
			}
		} else if e.Sequence > activity {
			activity = e.Sequence
		}
	}
	return interrupted && terminal > 0 && terminal >= activity
}

func cloudTurnProgress(raw []json.RawMessage, start, end agentSeq) []json.RawMessage {
	ids := map[string]bool{}
	for _, data := range raw {
		var patch agentTranscriptPatch
		if json.Unmarshal(data, &patch) == nil && patch.Op == "put" && patch.Entity.Sequence > start && (end == 0 || patch.Entity.Sequence <= end) {
			ids[patch.Entity.ID] = true
		}
	}
	var out []json.RawMessage
	for _, data := range raw {
		var patch agentTranscriptPatch
		if json.Unmarshal(data, &patch) != nil {
			continue
		}
		if patch.Op == "put" && ids[patch.Entity.ID] || (patch.Op == "patch" || patch.Op == "remove") && ids[patch.ID] {
			out = append(out, data)
		}
	}
	return out
}

func cloudBody(a *Account, in cloudRequest, r *cloudRecord) (string, map[string]interface{}) {
	endpoint := "createAgentThread"
	b := agentThreadBodyFromPrompt(a, in.Task, r.Model, r.ThreadID, r.MessageID)
	if in.ThreadID != "" {
		endpoint = "sendEventToAgentThread"
		b = agentThreadFollowUpBody(a, in.Task, r.Model, r.Effort, r.ThreadID, r.MessageID, nil)
	}
	b["reasoningEffort"] = r.Effort
	b["browserEnabled"] = true
	b["policies"] = map[string]string{"approval_mode": "yolo"}
	// These are native personal-Agent fields. Tool/skill selections are owned
	// by Notion; no local tool definitions or fabricated sandbox are injected.
	delete(b, "enableSuggestedEditsTools")
	return endpoint, b
}
func cloudJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func cloudError(w http.ResponseWriter, status int, code string) {
	cloudJSON(w, status, map[string]string{"error": code})
}
func cloudResult(r *cloudRecord) map[string]interface{} {
	return map[string]interface{}{"requestId": r.RequestID, "threadId": r.ThreadID, "threadUrl": "https://app.notion.com/chat?t=" + strings.ReplaceAll(r.ThreadID, "-", "") + "&wfv=chat", "status": r.State, "sent": map[string]interface{}{"model": r.Model, "reasoningEffort": r.Effort, "approval_mode": "yolo", "browserEnabled": true}, "server_effective": "unknown"}
}
func (c *CloudAgent) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// API-key middleware covers /v1/. Do not expose this journal through Dashboard.
	if req.Method != "POST" && req.Method != "GET" {
		w.Header().Set("Allow", "GET, POST")
		cloudError(w, 405, "method_not_allowed")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if req.Context().Err() != nil {
		cloudError(w, 408, "client_gone_before_dispatch")
		return
	}
	if c.loadErr != nil {
		cloudError(w, 503, "journal_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	if req.Method == "GET" {
		r := c.records[req.URL.Query().Get("requestId")]
		if r == nil && req.URL.Query().Get("threadId") != "" {
			r = c.latest(req.URL.Query().Get("threadId"))
		}
		if r == nil {
			cloudError(w, 404, "unknown_request_or_thread")
			return
		}
		a := c.account(r)
		if a == nil {
			cloudError(w, 409, "account_or_workspace_changed")
			return
		}
		if r.State == "rejected" {
			cloudJSON(w, 200, cloudResult(r))
			return
		}
		p, progress, err := c.transcript(ctx, a, r)
		if err != nil {
			cloudError(w, 502, "transcript_unavailable")
			return
		}
		if r.State == "completed" || r.State == "interrupted" {
			p = cloudCompletedPage(p, r.LastSeq)
		}
		text, done, seq, err := readAgentTranscriptSince(p, r.Baseline)
		if r.State == "completed" && (err != nil || !done) {
			cloudError(w, 502, "completed_turn_unreadable")
			return
		}
		var te *agentTranscriptError
		if err != nil {
			if !errors.As(err, &te) || te.kind == "transcript_format" {
				cloudError(w, 502, "transcript_unreadable")
				return
			}
			r.State = "failed"
			if cloudInterrupted(p, r.Baseline) {
				r.State = "interrupted"
			}
			r.LastSeq = seq
		} else if done {
			r.State = "completed"
			r.LastSeq = seq
		} else if p.Session.Status == "running" {
			r.State = "running"
		}
		if err := c.save(); err != nil {
			c.loadErr = err
			cloudError(w, 503, "journal_unavailable")
			return
		}
		result := cloudResult(r)
		result["text"] = text
		result["sessionStatus"] = p.Session.Status
		end := agentSeq(0)
		if r.State == "completed" || r.State == "failed" || r.State == "interrupted" {
			end = r.LastSeq
		}
		result["progress"] = cloudTurnProgress(progress, r.Baseline, end) // Native action receipts, not inferred from prose.
		cloudJSON(w, 200, result)
		return
	}
	var in cloudRequest
	d := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil || strings.TrimSpace(in.Task) == "" || len(in.RequestID) < 8 || len(in.RequestID) > 128 || !agentErrorCodePattern.MatchString(in.RequestID) {
		cloudError(w, 400, "invalid_request")
		return
	}
	if in.Model == "" {
		in.Model = "sonnet-5.5"
	}
	model, err := agentThreadModelID(in.Model)
	if err != nil {
		cloudError(w, 400, "model_unavailable")
		return
	}
	if in.Effort == "" {
		in.Effort = "high"
	}
	if in.Effort != "low" && in.Effort != "medium" && in.Effort != "high" {
		cloudError(w, 400, "invalid_effort")
		return
	}
	if strings.TrimSpace(in.Category) != "" && !isValidAgentTitleCategory(in.Category) {
		cloudError(w, 400, "invalid_category")
		return
	}
	var normalized []byte
	if strings.TrimSpace(in.Title) == "" && strings.TrimSpace(in.Category) == "" {
		normalized, _ = json.Marshal(struct{ Account, Thread, Task, Model, Effort string }{in.AccountID, in.ThreadID, in.Task, model, in.Effort})
	} else {
		normalized, _ = json.Marshal(struct{ Account, Thread, Task, Model, Effort, Title, Category string }{in.AccountID, in.ThreadID, in.Task, model, in.Effort, strings.TrimSpace(in.Title), strings.TrimSpace(in.Category)})
	}
	hash := cloudHash(string(normalized))
	if r := c.records[in.RequestID]; r != nil {
		if r.Fingerprint != hash {
			cloudError(w, 409, "request_id_conflict")
			return
		}
		cloudJSON(w, 200, cloudResult(r))
		return
	}
	var a *Account
	var baseline agentSeq
	if in.ThreadID != "" {
		prev := c.latest(in.ThreadID)
		if prev == nil {
			cloudError(w, 404, "unowned_thread")
			return
		}
		if in.AccountID != "" && in.AccountID != prev.AccountID {
			cloudError(w, 409, "account_or_workspace_changed")
			return
		}
		a = c.account(prev)
		if a == nil {
			cloudError(w, 409, "account_or_workspace_changed")
			return
		}
		if prev.State == "interrupted" {
			// Revalidate immediately before sending; another browser turn may
			// have started since the operator last queried the request.
			page, _, readErr := c.transcript(ctx, a, prev)
			if readErr != nil || !cloudInterrupted(page, prev.Baseline) {
				cloudError(w, 409, "interrupted_turn_not_idle")
				return
			}
			_, _, seq, _ := readAgentTranscriptSince(page, prev.Baseline)
			baseline = seq
		} else if prev.State != "completed" {
			cloudError(w, 409, "previous_turn_pending_query_first")
			return
		} else {
			baseline = prev.LastSeq
		}
	} else {
		if in.AccountID == "" {
			cloudError(w, 400, "account_id_required")
			return
		}
		a = c.pool.FindByAccountID(in.AccountID)
		if a == nil || a.SpaceID == "" {
			cloudError(w, 404, "account_unavailable")
			return
		}
	}
	r := &cloudRecord{RequestID: in.RequestID, Fingerprint: hash, AccountID: a.AccountID, SpaceID: a.SpaceID, ThreadID: in.ThreadID, MessageID: generateUUIDv4(), Model: model, Effort: in.Effort, State: "submission_unknown", Baseline: baseline}
	if r.ThreadID == "" {
		r.ThreadID = generateUUIDv4()
	}
	c.records[in.RequestID] = r
	if err := c.save(); err != nil {
		delete(c.records, in.RequestID)
		c.loadErr = err
		cloudError(w, 503, "journal_unavailable")
		return
	}
	endpoint, body := cloudBody(a, in, r)
	err = agentPost(ctx, c.client, a, endpoint, body, nil)
	status := http.StatusAccepted
	if err == nil {
		r.State = "submitted"
		if in.ThreadID == "" {
			setAgentThreadTitle(ctx, c.client, a, r.ThreadID, buildCloudAgentTitle(in.Title, in.Category, in.Task))
		}
	} else {
		var he *agentHTTPError
		if errors.As(err, &he) && he.definitiveRejection() {
			r.State = "rejected"
			status = 502
		}
	}
	if err := c.save(); err != nil {
		c.loadErr = err
		cloudError(w, 503, "journal_unavailable")
		return
	}
	result := cloudResult(r)
	if err != nil {
		result["error"] = "submission_not_confirmed"
		var he *agentHTTPError
		if errors.As(err, &he) {
			result["upstreamStatus"] = he.Status
			result["upstreamCode"] = he.Code
		}
	}
	cloudJSON(w, status, result)
}

var _ http.Handler = (*CloudAgent)(nil)
