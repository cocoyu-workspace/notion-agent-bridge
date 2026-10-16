package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Issue #28 acceptance fixture, one deterministic low-cost chain:
//
//	main Agent -> child reads a random file -> Edit -> Bash (failing test) ->
//	corrected Edit -> Bash (passing) -> main resumes the SAME child -> more edits ->
//	main reports the exact diff and test evidence.
//
// What each layer proves:
//   - MODEL: simulated. The fake Notion service replays a fixed script; the
//     script deliberately uses over-escaped, entity-escaped and plain tool text.
//   - BRIDGE: real code under test (agent transport, bindings, strict tool-text
//     guard, identity per child).
//   - LOCAL TOOLS: real. Read/Edit/Bash below act on a real temp Go module and run
//     the real `go test`; success is judged from files and process output, never
//     from HTTP 200, tool counts or the scripted model's words.
//   - UPSTREAM / real Claude CLI: NOT exercised here.

type fixtureTools struct {
	t   *testing.T
	dir string
	mu  sync.Mutex
	log []string // tool name per executed call
}

func (x *fixtureTools) exec(call ToolCall) string {
	x.mu.Lock()
	x.log = append(x.log, call.Function.Name)
	x.mu.Unlock()
	var args map[string]string
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		x.t.Fatalf("bridge delivered undecodable arguments: %q", call.Function.Arguments)
	}
	switch call.Function.Name {
	case "Read":
		b, err := os.ReadFile(filepath.Join(x.dir, args["file_path"]))
		if err != nil {
			return "ERROR: " + err.Error()
		}
		return string(b)
	case "Edit":
		p := filepath.Join(x.dir, args["file_path"])
		b, err := os.ReadFile(p)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		if strings.Count(string(b), args["old_string"]) != 1 {
			return "ERROR: old_string must match exactly once"
		}
		out := strings.Replace(string(b), args["old_string"], args["new_string"], 1)
		if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
			return "ERROR: " + err.Error()
		}
		return "edited " + args["file_path"]
	case "Bash":
		cmd := exec.Command("bash", "-c", args["command"])
		cmd.Dir = x.dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Sprintf("exit=%v\n%s", err, out)
		}
		return "exit=0\n" + string(out)
	}
	x.t.Fatalf("unexpected tool %q", call.Function.Name)
	return ""
}

func fixtureDeclared() declaredTools {
	return declaredTools{
		"Read": {Required: []string{"file_path"}}, "Edit": {Required: []string{"file_path", "old_string", "new_string"}},
		"Bash": {Required: []string{"command"}}, "Agent": {Required: []string{"prompt"}}, "SendMessage": {Required: []string{"to", "message"}},
	}
}

func jsonCall(name string, args map[string]string) string {
	b, _ := json.Marshal(map[string]interface{}{"name": name, "arguments": args})
	return string(b)
}

func TestAgentFixtureChainMainChildEditBashResume(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	rnd := make([]byte, 8)
	rand.Read(rnd)
	token := "TOKEN-" + hex.EncodeToString(rnd)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fx\n\ngo 1.21\n")
	write("calc.go", "package fx\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	write("calc_test.go", "package fx\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatalf(\"Add(2,3)=%d\", Add(2, 3))\n\t}\n}\n")
	write("notes.txt", "task: Add must add. marker="+token+"\n")
	origDir := t.TempDir()
	origPath := filepath.Join(origDir, "calc.go")
	if err := os.WriteFile(origPath, []byte("package fx\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	esc := func(s string) string { return strings.ReplaceAll(s, `"`, `\"`) }
	ent := func(s string) string { return strings.NewReplacer(`"`, "&quot;", "<", "&lt;", ">", "&gt;").Replace(s) }
	script := []string{
		/*1 main */ jsonCall("Agent", map[string]string{"description": "fix Add", "prompt": "read notes.txt and fix Add"}),
		/*2 child*/ esc(jsonCall("Read", map[string]string{"file_path": "notes.txt"})),
		/*3 child*/ ent(jsonCall("Edit", map[string]string{"file_path": "calc.go", "old_string": "return a - b", "new_string": "return a * b"})),
		/*4 child*/ jsonCall("Bash", map[string]string{"command": "go test ./... 2>&1 | tail -n 8"}),
		/*5 child*/ esc(jsonCall("Edit", map[string]string{"file_path": "calc.go", "old_string": "return a * b", "new_string": "return a + b"})),
		/*6 child*/ jsonCall("Bash", map[string]string{"command": "go test ./... 2>&1 | tail -n 8"}),
		/*7 child*/ "Fixed Add; go test passes.",
		/*8 main */ jsonCall("SendMessage", map[string]string{"to": "child-1", "message": "also document Add"}),
		/*9 child*/ ent(jsonCall("Edit", map[string]string{"file_path": "calc.go", "old_string": "func Add(", "new_string": "// Add returns a+b.\nfunc Add("})),
		/*10child*/ jsonCall("Bash", map[string]string{"command": "go test ./... 2>&1 | tail -n 8"}),
		/*11child*/ "Documented Add; tests pass.",
		/*12main */ "REPORT: see diff above.",
	}

	f := newFakeAgentServer(t)
	f.replyFn = func(n int) string { return script[n-1] }
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	tools := &fixtureTools{t: t, dir: dir}
	declared := fixtureDeclared()
	mainKey := agentClientKey("main-session")
	childKey := agentClientKey("child-1-of-main-session")

	// turn runs one model step and returns the guard verdict (the bridge's real classification).
	turn := func(key string, msgs []ChatMessage) toolTextVerdict {
		text, err := runKeyed(t, f, store, acc, key, "low", true, msgs)
		if err != nil {
			t.Fatalf("bridge error: %v", err)
		}
		v := guardToolText(text, declared)
		if v.Kind == toolTextMalformed {
			t.Fatalf("scripted valid call classified malformed: %v", v.Malformed)
		}
		return v
	}
	// drive executes tool calls for real until the model answers in prose.
	drive := func(key string, msgs []ChatMessage, firstTool func(ToolCall) (string, bool)) ([]ChatMessage, string, []string) {
		var results []string
		for step := 0; step < 12; step++ {
			v := turn(key, msgs)
			if v.Kind == toolTextPlain {
				return msgs, v.Remaining, results
			}
			msgs = append(msgs, ChatMessage{Role: "assistant", ToolCalls: v.Calls})
			for _, c := range v.Calls {
				var out string
				if firstTool != nil {
					if o, handled := firstTool(c); handled {
						out = o
					}
				}
				if out == "" {
					out = tools.exec(c)
				}
				results = append(results, out)
				msgs = append(msgs, ChatMessage{Role: "tool", ToolCallID: c.ID, Name: c.Function.Name, Content: out})
			}
		}
		t.Fatal("tool loop did not terminate")
		return nil, "", nil
	}

	// Main thread asks for a child; the child's own loop runs with real tools.
	mainMsgs := []ChatMessage{sysMsg("main system"), u("MAIN-TASK: delegate the Add fix")}
	v := turn(mainKey, mainMsgs)
	if len(v.Calls) != 1 || v.Calls[0].Function.Name != "Agent" {
		t.Fatalf("main did not request the Agent tool: %+v", v)
	}
	mainMsgs = append(mainMsgs, ChatMessage{Role: "assistant", ToolCalls: v.Calls})

	childMsgs := []ChatMessage{sysMsg("subagent system"), u("CHILD-TASK: read notes.txt and fix Add")}
	childMsgs, childFinal, results := drive(childKey, childMsgs, nil)
	if !strings.Contains(results[0], token) {
		t.Fatalf("child's Read did not return the random file content: %q", results[0])
	}
	if !strings.Contains(results[2], "FAIL") {
		t.Fatalf("the deliberately wrong fix must fail a REAL go test run, got %q", results[2])
	}
	if !strings.Contains(results[4], "ok") || strings.Contains(results[4], "FAIL") {
		t.Fatalf("corrected fix must pass the real go test run, got %q", results[4])
	}
	// The child's random token must have reached the child thread (proof the Read result was carried), not the main thread.
	if c, s := f.counts(); c != 2 || s != 5 {
		t.Fatalf("after child loop: creates=%d sends=%d, want 2/5", c, s)
	}
	for i, c := range f.creates {
		raw, _ := json.Marshal(c)
		if i == 1 && strings.Contains(string(raw), "MAIN-TASK") {
			t.Fatal("child thread was created from main history")
		}
	}

	// Main receives the child's result and resumes the SAME child via SendMessage.
	mainMsgs = append(mainMsgs, ChatMessage{Role: "tool", ToolCallID: v.Calls[0].ID, Name: "Agent", Content: childFinal})
	v = turn(mainKey, mainMsgs)
	if len(v.Calls) != 1 || v.Calls[0].Function.Name != "SendMessage" {
		t.Fatalf("main did not resume the child: %+v", v)
	}
	mainMsgs = append(mainMsgs, ChatMessage{Role: "assistant", ToolCalls: v.Calls})
	childMsgs = append(childMsgs, ChatMessage{Role: "assistant", Content: childFinal}, u("also document Add"))
	childMsgs, childFinal2, _ := drive(childKey, childMsgs, nil)

	// Real files and a real diff drive the main report.
	got, _ := os.ReadFile(filepath.Join(dir, "calc.go"))
	if !strings.Contains(string(got), "return a + b") || !strings.Contains(string(got), "// Add returns a+b.") {
		t.Fatalf("edits did not land on disk:\n%s", got)
	}
	diffOut, _ := exec.Command("diff", "-u", origPath, filepath.Join(dir, "calc.go")).CombinedOutput()
	diff := string(diffOut)
	if !strings.Contains(diff, "+\treturn a + b") || !strings.Contains(diff, "-\treturn a - b") {
		t.Fatalf("unexpected real diff:\n%s", diff)
	}
	mainMsgs = append(mainMsgs, ChatMessage{Role: "tool", ToolCallID: v.Calls[0].ID, Name: "SendMessage", Content: childFinal2 + "\n" + diff})
	v = turn(mainKey, mainMsgs)
	if v.Kind != toolTextPlain {
		t.Fatalf("final main reply must be an ordinary answer: %+v", v)
	}

	// Thread accounting: one thread for main, one for the child, both resumed (never forked).
	if c, s := f.counts(); c != 2 || s != 10 || threadCount(f) != 2 {
		t.Fatalf("creates=%d sends=%d threads=%d, want 2/10/2", c, s, threadCount(f))
	}
	byThread := map[string]int{}
	for _, s := range f.sends {
		byThread[s["threadId"].(string)]++
	}
	if len(byThread) != 2 {
		t.Fatalf("sends must go to exactly the two bound threads: %v", byThread)
	}
	// The main thread's last prompt carries the real diff and the passing test evidence, byte for byte.
	last := f.sends[len(f.sends)-1]
	prompt := promptText(last["event"].(map[string]interface{})["content"])
	if !strings.Contains(prompt, diff) || !strings.Contains(prompt, "Documented Add; tests pass.") {
		t.Fatalf("main prompt lacks the real diff/evidence:\n%s", prompt)
	}
	wantTools := []string{"Read", "Edit", "Bash", "Edit", "Bash", "Edit", "Bash"}
	if strings.Join(tools.log, ",") != strings.Join(wantTools, ",") {
		t.Fatalf("executed tools %v, want %v", tools.log, wantTools)
	}
}
