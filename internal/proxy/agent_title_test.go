package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// --- merged from agent_title_test.go ---
func TestAgentThreadTitleFormattingAndCategories(t *testing.T) {
	oldNow := agentTitleNow
	agentTitleNow = func() time.Time { return time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC) }
	defer func() { agentTitleNow = oldNow }()

	got := formatAgentThreadTitle("本", "调试", truncateAgentRunes("MUL-485 排查 Local Client Action 标题被覆盖问题并且验证长度", agentTitleTopicMaxRunes))
	if !strings.HasPrefix(got, "1014 | 本 | 调试 | MUL-485 ") {
		t.Fatalf("unexpected formatted title: %q", got)
	}
	parts := strings.Split(got, " | ")
	if len(parts) != 4 || utf8.RuneCountInString(parts[3]) > agentTitleTopicMaxRunes {
		t.Fatalf("topic rune count exceeded %d: %q (%d)", agentTitleTopicMaxRunes, parts[3], utf8.RuneCountInString(parts[3]))
	}

	cases := []struct {
		text string
		want string
	}{
		{"帮我排查一下 MUL-485 报错原因", "调试"},
		{"优化一下 token 缓存命中率", "优化"},
		{"重构并清理冗余的 session 绑定逻辑", "整改"},
		{"出一份新的架构设计方案和 spec", "设计"},
		{"修改 config.yaml 里的端口与环境变量配置", "配置"},
		{"更新 README.md 和接口说明文档", "文档"},
		{"准备 #28 发版上线验收", "发布"},
		{"调研并对比本周各渠道调用情况", "分析"},
		{"新增支持自定义会话标题能力", "功能"},
		{"hello world", "杂项"},
	}
	for _, tc := range cases {
		if cat := inferAgentTitleCategory(tc.text); cat != tc.want {
			t.Fatalf("inferAgentTitleCategory(%q) = %q, want %q", tc.text, cat, tc.want)
		}
	}

	rawPrompt := "<system-reminder>\nInternal hook\n</system-reminder>\n# 请帮我查一下\n详情见 !1024 合并请求"
	topic := extractAgentTitleTopic(rawPrompt)
	if topic != "!1024 请帮我查一下" {
		t.Fatalf("unexpected extracted topic: %q", topic)
	}
}

func TestAgentThreadTitleChannelsAndRebindTakeover(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	mmdd := agentTitleNow().Format("0102")

	// 1. Tool-less startup helper (e.g. Claude Code's Coding Session Title Schema)
	// creates a thread first and gets tagged as "旁".
	var out strings.Builder
	err := callAgentThreadResumableSubagent(
		nil,
		acc,
		[]ChatMessage{u("Generate a short coding session title")},
		"",
		"low",
		"client-main",
		"",
		false, // tooled = false
		func(text string, done bool, _ *UsageInfo) { out.WriteString(text) },
		f.server.Client(),
		store,
	)
	if err != nil {
		t.Fatalf("helper turn failed: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("expected 1 created thread, got %d", len(f.creates))
	}
	threadID := f.creates[0]["threadId"].(string)
	wantHelper := mmdd + " | 旁 | 杂项 | Generate a short coding session"
	if got := f.titles[threadID]; got != wantHelper || !f.userSetTitle[threadID] {
		t.Fatalf("helper title = %q (user_set_title=%v), want %q", got, f.userSetTitle[threadID], wantHelper)
	}

	// 2. Real tool-using main conversation arrives on the same clientKey and takes
	// over the helper thread via rebindCandidate; its title must be overwritten to "本".
	out.Reset()
	err = callAgentThreadResumableSubagent(
		nil,
		acc,
		[]ChatMessage{u("MUL-485 排查会话标题被覆盖的 bug")},
		"",
		"low",
		"client-main",
		"",
		true, // tooled = true
		func(text string, done bool, _ *UsageInfo) { out.WriteString(text) },
		f.server.Client(),
		store,
	)
	if err != nil {
		t.Fatalf("main takeover turn failed: %v", err)
	}
	wantMain := mmdd + " | 本 | 调试 | MUL-485 排查会话标题被覆盖的 bug"
	if got := f.titles[threadID]; got != wantMain || !f.userSetTitle[threadID] {
		t.Fatalf("main takeover title = %q, want %q", got, wantMain)
	}

	// 3. Subagent conversation carrying X-Claude-Code-Agent-Id gets tagged "子#xxxx".
	out.Reset()
	err = callAgentThreadResumableSubagent(
		nil,
		acc,
		[]ChatMessage{u("检索 memex 知识库里的协议记录")},
		"",
		"low",
		"client-sub",
		"a1b2c3d4-9999",
		true,
		func(text string, done bool, _ *UsageInfo) { out.WriteString(text) },
		f.server.Client(),
		store,
	)
	if err != nil {
		t.Fatalf("subagent turn failed: %v", err)
	}
	subThreadID := f.creates[1]["threadId"].(string)
	wantSub := mmdd + " | 子#a1b2 | 设计 | 检索 memex 知识库里的协议记录"
	if got := f.titles[subThreadID]; got != wantSub {
		t.Fatalf("subagent title = %q, want %q", got, wantSub)
	}

	// 4. Recap helper request gets tagged "旁 | 杂项 | 暂离回归回顾".
	recapTitle := agentTurnTitle(agentPlanInput{
		Recap:    true,
		Messages: []ChatMessage{u("summarize what happened while away")},
	})
	wantRecap := mmdd + " | 旁 | 杂项 | 暂离回归回顾"
	if recapTitle != wantRecap {
		t.Fatalf("recap title = %q, want %q", recapTitle, wantRecap)
	}
}

func TestAgentCloudTitleAutoAndExplicit(t *testing.T) {
	f := newFakeAgentServer(t)
	pool := NewAccountPool()
	acc := &Account{UserID: "user", SpaceID: "space", TokenV2: "secret-cookie"}
	pool.AddAccount(acc)
	acc.EnsureAccountID()
	c := NewCloudAgent(pool, t.TempDir())
	c.client = f.server.Client()
	defer c.Close()

	call := func(body string) (int, map[string]interface{}) {
		t.Helper()
		w := httptest.NewRecorder()
		c.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/cloud-agent", strings.NewReader(body)))
		var v map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}

	mmdd := agentTitleNow().Format("0102")

	// 1. Default auto-generated title for Cloud Agent ("云").
	code, resp1 := call(`{"requestId":"cloud-title-001","accountId":"` + acc.AccountID + `","task":"调研并对比本周工作进展"}`)
	if code != http.StatusAccepted {
		t.Fatalf("cloud-title-001 status=%d body=%v", code, resp1)
	}
	th1 := resp1["threadId"].(string)
	want1 := mmdd + " | 云 | 分析 | 调研并对比本周工作进展"
	if got := f.titles[th1]; got != want1 || !f.userSetTitle[th1] {
		t.Fatalf("cloud auto title = %q (user_set_title=%v), want %q", got, f.userSetTitle[th1], want1)
	}

	// 2. Explicit category + topic on Cloud Agent.
	code, resp2 := call(`{"requestId":"cloud-title-002","accountId":"` + acc.AccountID + `","category":"发布","title":"NCB #28 发版验收","task":"执行完整回归验证"}`)
	if code != http.StatusAccepted {
		t.Fatalf("cloud-title-002 status=%d body=%v", code, resp2)
	}
	th2 := resp2["threadId"].(string)
	want2 := mmdd + " | 云 | 发布 | NCB #28 发版验收"
	if got := f.titles[th2]; got != want2 {
		t.Fatalf("cloud explicit title = %q, want %q", got, want2)
	}

	// 3. Invalid category rejected with 400.
	code, resp3 := call(`{"requestId":"cloud-title-003","accountId":"` + acc.AccountID + `","category":"不存在的分类","task":"test"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid category, got %d (%v)", code, resp3)
	}
}

// --- merged from agent_helper_abandoned_test.go ---
// Issue #61: Claude Code's tool-less title query was bound first and its turn
// never completed from the bridge's view (client abandoned it). The main
// conversation then got 409 [bridge:pending] on every retry.
func TestAgentBindingAbandonedHelperDoesNotWedgeMain(t *testing.T) {
	for _, st := range []string{agentStateSubmitting, agentStateSubmitted} {
		t.Run(st, func(t *testing.T) { abandonedHelperCase(t, st) })
	}
}

func abandonedHelperCase(t *testing.T, state string) {
	f, store, acc := bindingSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", false, []ChatMessage{u("title please")}); err != nil {
		t.Fatal(err)
	}
	in := agentPlanInput{ClientKey: "s", AccountID: agentAccountKey(acc), SpaceID: "space"}
	store.mu.Lock()
	helper := store.primaryLocked(in)
	helper.State = state // cancelled before Notion confirmed it, or its reply was never collected
	helperID := helper.ThreadID
	store.mu.Unlock()

	m1 := []ChatMessage{sysMsg("tools"), u("real work")}
	out, err := runKeyed(t, f, store, acc, "s", "low", true, m1)
	if err != nil || out == "" {
		t.Fatalf("main request must not be refused as pending: %q %v", out, err)
	}
	if c, _ := f.counts(); c != 2 {
		t.Fatalf("main must get its own thread: creates=%d", c)
	}
	store.mu.Lock()
	main := store.primaryLocked(in)
	_, helperKept := store.records[helperID]
	store.mu.Unlock()
	if main.ThreadID == helperID || helperKept || !main.Tooled {
		t.Fatalf("binding: main=%s helper=%s kept=%v", shortID(main.ThreadID), shortID(helperID), helperKept)
	}
	// The main conversation continues on its own thread.
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, append(append([]ChatMessage{}, m1...), a(out), u("more"))); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 2 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	// A late helper retry cannot take the binding back or create a chat.
	_, err = runKeyed(t, f, store, acc, "s", "low", false, []ChatMessage{u("title please")})
	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "diverged" {
		t.Fatalf("got %v", err)
	}
	if c, _ := f.counts(); c != 2 {
		t.Fatalf("creates=%d", c)
	}
}

// --- merged from agent_recap_test.go ---
const testAwayRecapPrompt = "The user stepped away and is coming back. Recap in under 40 words, 1-2 plain sentences, no markdown. Lead with the overall goal and current task, then the one next action. Skip root-cause narrative, fix internals, secondary to-dos, and em-dash tangents."

func TestAgentAwayRecapPreservesMainBinding(t *testing.T) {
	f, store, acc := bindingSetup(t)
	key := "recap-main"
	first := []ChatMessage{u("hello")}
	if _, err := runKeyed(t, f, store, acc, key, "low", true, first); err != nil {
		t.Fatal(err)
	}
	var before agentThreadRecord
	for _, r := range store.records {
		before = *r
	}
	recap := []ChatMessage{u("hello"), a("hello back"), u(testAwayRecapPrompt)}
	if out, err := runKeyed(t, f, store, acc, key, "low", false, recap); err != nil || out != "reply-2" {
		t.Fatalf("recap %q %v", out, err)
	}
	if !reflect.DeepEqual(before, *store.records[before.ThreadID]) {
		t.Fatal("recap changed main binding")
	}
	actual := []ChatMessage{u("hello"), a("hello back"), u("Now read my project file")}
	if out, err := runKeyed(t, f, store, acc, key, "low", true, actual); err != nil || out != "reply-3" {
		t.Fatalf("normal follow-up after recap %q %v", out, err)
	}
	if c, s := f.counts(); c != 2 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	if f.sends[0]["threadId"] != before.ThreadID {
		t.Fatal("main follow-up went to another thread")
	}
}

func TestAgentAwayRecapRetryAndRestart(t *testing.T) {
	f := newFakeAgentServer(t)
	path := filepath.Join(t.TempDir(), "state")
	store := newAgentThreadStore(path)
	acc := &Account{UserID: "user", SpaceID: "space"}
	key := "main-restart"
	msgs := []ChatMessage{u("hello"), a("hello back"), u(testAwayRecapPrompt)}
	if out, err := runKeyed(t, f, store, acc, key, "low", false, msgs); err != nil || out != "reply-1" {
		t.Fatalf("first %q %v", out, err)
	}
	store = newAgentThreadStore(path)
	if out, err := runKeyed(t, f, store, acc, key, "low", false, msgs); err != nil || out != "reply-1" {
		t.Fatalf("retry %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 0 {
		t.Fatalf("retry duplicated recap: %d/%d", c, s)
	}
	if _, err := runKeyed(t, f, store, acc, key, "low", true, []ChatMessage{u("hello")}); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.counts(); c != 2 {
		t.Fatal("recap-first claimed main binding")
	}
}

func TestAgentAwayRecapDoesNotGuessOrdinaryToollessRequests(t *testing.T) {
	for _, text := range []string{"Explain the result without tools", "Quoted: " + testAwayRecapPrompt, testAwayRecapPrompt + " Also do something else."} {
		t.Run(text[:12], func(t *testing.T) {
			f, store, acc := bindingSetup(t)
			first := []ChatMessage{u("hello")}
			if _, err := runKeyed(t, f, store, acc, "main", "low", true, first); err != nil {
				t.Fatal(err)
			}
			next := []ChatMessage{u("hello"), a("hello back"), u(text)}
			if _, err := runKeyed(t, f, store, acc, "main", "low", false, next); err != nil {
				t.Fatal(err)
			}
			if c, s := f.counts(); c != 1 || s != 1 {
				t.Fatalf("ordinary request routed away: %d/%d", c, s)
			}
		})
	}
}

func TestAgentAwayRecapIdentityUsesActualReplyAndParent(t *testing.T) {
	f, store, acc := bindingSetup(t)
	a1 := []ChatMessage{u("hello"), a("reply A"), u(testAwayRecapPrompt)}
	a2 := []ChatMessage{u("hello"), a("reply B"), u(testAwayRecapPrompt)}
	for _, req := range []struct {
		key  string
		msgs []ChatMessage
	}{{"parent-a", a1}, {"parent-a", a2}, {"parent-b", a1}} {
		if _, err := runKeyed(t, f, store, acc, req.key, "low", false, req.msgs); err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := f.counts(); c != 3 {
		t.Fatalf("distinct recap histories/parents merged: creates=%d", c)
	}
}

func TestAgentAwayRecapConcurrentMainAndRecap(t *testing.T) {
	f, store, acc := bindingSetup(t)
	if _, err := runKeyed(t, f, store, acc, "main", "low", true, []ChatMessage{u("hello")}); err != nil {
		t.Fatal(err)
	}
	var mainID string
	for id := range store.records {
		mainID = id
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, req := range []struct {
		tooled bool
		msgs   []ChatMessage
	}{{false, []ChatMessage{u("hello"), a("hello back"), u(testAwayRecapPrompt)}}, {true, []ChatMessage{u("hello"), a("hello back"), u("Next task")}}} {
		wg.Add(1)
		go func(req struct {
			tooled bool
			msgs   []ChatMessage
		}) {
			defer wg.Done()
			_, err := runKeyed(t, f, store, acc, "main", "low", req.tooled, req.msgs)
			errs <- err
		}(req)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if c, s := f.counts(); c != 2 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	if f.sends[0]["threadId"] != mainID {
		t.Fatal("main sent to recap thread")
	}
}

func TestAgentAwayRecapTransientPollKeepsMainAndReusesAuxiliary(t *testing.T) {
	f, store, acc := bindingSetup(t)
	if _, err := runKeyed(t, f, store, acc, "main", "low", true, []ChatMessage{u("hello")}); err != nil {
		t.Fatal(err)
	}
	var before agentThreadRecord
	for _, r := range store.records {
		before = *r
	}
	AppConfig.Timeouts.InferenceTimeout = 1
	f.stall = true
	msgs := []ChatMessage{u("hello"), a("hello back"), u(testAwayRecapPrompt)}
	if _, err := runKeyed(t, f, store, acc, "main", "low", false, msgs); err == nil {
		t.Fatal("expected wait deadline")
	}
	if !reflect.DeepEqual(before, *store.records[before.ThreadID]) {
		t.Fatal("pending recap changed main")
	}
	f.mu.Lock()
	f.stall = false
	for _, th := range f.threads {
		f.finishLocked(th)
	}
	f.mu.Unlock()
	if _, err := runKeyed(t, f, store, acc, "main", "low", false, msgs); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 2 || s != 0 {
		t.Fatalf("pending recap duplicated: %d/%d", c, s)
	}
	if !reflect.DeepEqual(before, *store.records[before.ThreadID]) {
		t.Fatal("recovered recap changed main")
	}
}
