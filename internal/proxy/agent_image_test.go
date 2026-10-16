package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// --- merged from agent_image_test.go ---
func TestAgentImageUploadCurrentProtocolAndCredentialIsolation(t *testing.T) {
	oldCfg, oldBase := AppConfig, NotionAPIBase
	AppConfig = DefaultConfig()
	defer func() { AppConfig, NotionAPIBase = oldCfg, oldBase }()
	stored := false
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("signed upload leaked credentials or used wrong method")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "image-bytes" || r.Header.Get("x-amz-tagging") != "test-tag" {
			t.Error("file bytes or signed headers changed")
		}
		stored = true
		w.WriteHeader(200)
	}))
	defer store.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["spaceId"] != "space" || !strings.Contains(r.Header.Get("Cookie"), "test-cookie") {
			t.Error("Notion account scope missing")
		}
		switch r.URL.Path {
		case "/createAgentServiceFileUploadURL":
			if _, ok := body["pointer"]; ok {
				t.Error("legacy thread pointer sent")
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"type": "file_upload_url", "file": map[string]string{"id": "file-id"}, "upload": map[string]interface{}{"type": "single_part", "method": "PUT", "url": store.URL, "headers": map[string]string{"x-amz-tagging": "test-tag"}}})
		case "/completeAgentServiceFileUpload":
			if !stored || body["fileId"] != "file-id" {
				t.Error("completion before successful upload")
			}
			io.WriteString(w, `{"type":"file","id":"file-id"}`)
		default:
			t.Error("unexpected API endpoint")
		}
	}))
	defer api.Close()
	NotionAPIBase = api.URL
	result, err := uploadAgentImageWithClients(&Account{SpaceID: "space", TokenV2: "test-cookie"}, &FileAttachment{FileName: "test.png", ContentType: "image/png", Data: []byte("image-bytes")}, api.Client(), store.Client())
	if err != nil || result.AgentFileID != "file-id" {
		t.Fatalf("%v %v", result, err)
	}
}

func TestAgentImageUploadRejectsNonImageBeforeNetwork(t *testing.T) {
	_, err := uploadAgentImageWithClients(&Account{}, &FileAttachment{ContentType: "application/pdf", Data: []byte("pdf")}, nil, nil)
	if err == nil {
		t.Fatal("unsupported document accepted")
	}
}

type failedSignedUploadTransport struct{}

func (failedSignedUploadTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Put", URL: "https://storage.test/?signature=secret", Err: errors.New("connection reset")}
}

func TestAgentImageUploadFailureDoesNotExposeSignedURL(t *testing.T) {
	oldCfg, oldBase := AppConfig, NotionAPIBase
	AppConfig = DefaultConfig()
	defer func() { AppConfig, NotionAPIBase = oldCfg, oldBase }()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"type":"file_upload_url","file":{"id":"file-id"},"upload":{"type":"single_part","method":"PUT","url":"https://storage.test/?signature=secret","headers":{}}}`)
	}))
	defer api.Close()
	NotionAPIBase = api.URL
	_, err := uploadAgentImageWithClients(&Account{}, &FileAttachment{ContentType: "image/png", Data: []byte("png")}, api.Client(), &http.Client{Transport: failedSignedUploadTransport{}})
	if err == nil || strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("signed URL was exposed: %v", err)
	}
}

func TestAgentThreadImageReferencesReachCreateRequest(t *testing.T) {
	oldCfg, oldBase := AppConfig, NotionAPIBase
	AppConfig = DefaultConfig()
	defer func() { AppConfig, NotionAPIBase = oldCfg, oldBase }()
	created := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/createAgentThread" {
			var body struct {
				Content []struct {
					Type   string `json:"type"`
					FileID string `json:"file_id"`
				} `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Content) != 2 || body.Content[0].Type != "text" || body.Content[1].FileID != "image-file" {
				t.Errorf("attachment reference lost: %+v", body)
			}
			created = true
			io.WriteString(w, `{}`)
		} else {
			io.WriteString(w, capturedShape)
		}
	}))
	defer api.Close()
	NotionAPIBase = api.URL
	err := callAgentThreadWithClient(&Account{}, nil, "", "", func(string, bool, *UsageInfo) {}, api.Client(), UploadedAttachment{AgentFileID: "image-file"})
	if err != nil || !created {
		t.Fatalf("%v created=%v", err, created)
	}
}

func TestAgentImageUploadRetriesTransientFailure(t *testing.T) {
	oldCfg, oldBase := AppConfig, NotionAPIBase
	AppConfig = DefaultConfig()
	defer func() { AppConfig, NotionAPIBase = oldCfg, oldBase }()

	putCalls := 0
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		putCalls++
		if putCalls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer store.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/createAgentServiceFileUploadURL":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "file_upload_url",
				"file": map[string]string{"id": "file-retry"},
				"upload": map[string]interface{}{
					"type":    "single_part",
					"method":  "PUT",
					"url":     store.URL,
					"headers": map[string]string{},
				},
			})
		case "/completeAgentServiceFileUpload":
			io.WriteString(w, `{"type":"file","id":"file-retry"}`)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
		}
	}))
	defer api.Close()
	NotionAPIBase = api.URL

	res, err := uploadAgentImageWithClients(&Account{SpaceID: "space", TokenV2: "cookie"}, &FileAttachment{
		FileName:    "retry.png",
		ContentType: "image/png",
		Data:        []byte("png-data"),
	}, api.Client(), store.Client())
	if err != nil || res == nil || res.AgentFileID != "file-retry" || putCalls != 2 {
		t.Fatalf("expected retry to succeed on 2nd PUT attempt, got res=%v err=%v putCalls=%d", res, err, putCalls)
	}
}

// --- merged from agent_image_resume_test.go ---
// Model the server's file ownership check: create consumes staged user files,
// whereas a follow-up can only reference files uploaded to that exact thread.
func TestAgentImageUploadsFollowSelectedThread(t *testing.T) {
	for _, key := range []string{agentClientKey("image-fixture"), ""} {
		name := "stable-client"
		if key == "" {
			name = "history-matched"
		}
		t.Run(name, func(t *testing.T) { testAgentImageUploadsFollowSelectedThread(t, key) })
	}
}

func testAgentImageUploadsFollowSelectedThread(t *testing.T, key string) {
	f := newFakeAgentServer(t)
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("credentials sent to storage")
		}
		io.Copy(io.Discard, r.Body)
	}))
	defer storage.Close()
	oldTransport := http.DefaultTransport
	http.DefaultTransport = storage.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	targets := map[string]map[string]interface{}{}
	uploads := 0
	failUpload := false
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "AgentServiceFileUpload") {
			if failUpload {
				w.WriteHeader(503)
				return
			}
			var b map[string]interface{}
			json.NewDecoder(r.Body).Decode(&b)
			target := b["target"].(map[string]interface{})
			if strings.HasSuffix(r.URL.Path, "createAgentServiceFileUploadURL") {
				uploads++
				id := fmt.Sprintf("file-%d", uploads)
				targets[id] = target
				json.NewEncoder(w).Encode(map[string]interface{}{"type": "file_upload_url", "file": map[string]string{"id": id}, "upload": map[string]interface{}{"type": "single_part", "method": "PUT", "url": storage.URL, "headers": map[string]string{}}})
			} else {
				id := b["fileId"].(string)
				if fmt.Sprint(target) != fmt.Sprint(targets[id]) {
					t.Error("completion target differs")
				}
				json.NewEncoder(w).Encode(map[string]string{"type": "file", "id": id})
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "sendEventToAgentThread") {
			data, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			var b map[string]interface{}
			json.Unmarshal(data, &b)
			for _, v := range b["event"].(map[string]interface{})["content"].([]interface{}) {
				c := v.(map[string]interface{})
				if c["type"] != "file" {
					continue
				}
				target := targets[fmt.Sprint(c["file_id"])]
				if target["type"] != "thread" || target["threadId"] != b["threadId"] {
					w.WriteHeader(400)
					io.WriteString(w, `{"name":"ValidationError","debugMessage":"file not found"}`)
					return
				}
			}
		}
		f.handle(w, r)
	})
	store := newAgentThreadStore("")
	acc := &Account{AccountID: "account", SpaceID: "space", TokenV2: "test-cookie"}
	raw := func(name string) UploadedAttachment {
		return UploadedAttachment{agentImage: &FileAttachment{FileName: name, ContentType: "image/png", Data: []byte("fixture")}}
	}
	first := []ChatMessage{u("first image")}
	run := func(msgs []ChatMessage, images ...UploadedAttachment) error {
		return callAgentThreadResumable(acc, msgs, "", "", key, false, func(string, bool, *UsageInfo) {}, f.server.Client(), store, images...)
	}
	// A failed upload on a fresh session submitted no remote create. It must
	// not occupy that session or prevent correcting the first request.
	failUpload = true
	if err := run(first, raw("first.png")); err == nil {
		t.Fatal("first upload failure ignored")
	}
	if len(store.records) != 0 || len(f.creates) != 0 {
		t.Fatal("unsubmitted first upload retained a binding")
	}
	failUpload = false
	if err := run(first, raw("first.png")); err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || targets["file-1"]["type"] != "user" {
		t.Fatalf("first upload target: %v count=%d", targets, uploads)
	}
	next := append(first, a("reply-1"), u("next image"))
	if err := run(next, raw("first.png"), raw("next.png")); err != nil {
		t.Fatal(err)
	}
	if uploads != 2 {
		t.Fatalf("historical image reuploaded: %d", uploads)
	}
	if len(f.creates) != 1 || len(f.sends) != 1 {
		t.Fatalf("creates=%d sends=%d", len(f.creates), len(f.sends))
	}
	if err := run(next, raw("first.png"), raw("next.png")); err != nil {
		t.Fatal(err)
	}
	if uploads != 2 || len(f.sends) != 1 {
		t.Fatal("replay uploaded or sent again")
	}
	var before agentThreadRecord
	for _, rec := range store.records {
		before = *rec
	}
	last := append(next, a("reply-2"), u("third image"))
	failUpload = true
	if err := run(last, raw("first.png"), raw("next.png"), raw("last.png")); err == nil {
		t.Fatal("upload failure ignored")
	}
	if !reflect.DeepEqual(before, *store.records[before.ThreadID]) {
		// A rollback refreshes UpdatedAt, but must retain all conversation state.
		after := *store.records[before.ThreadID]
		after.UpdatedAt = before.UpdatedAt
		if !reflect.DeepEqual(before, after) {
			t.Fatal("upload failure advanced binding")
		}
	}
	if len(f.creates) != 1 || len(f.sends) != 1 {
		t.Fatal("upload failure submitted a turn")
	}
	failUpload = false
	if err := run(last, raw("first.png"), raw("next.png"), raw("last.png")); err != nil {
		t.Fatal(err)
	}
	if uploads != 3 || len(f.creates) != 1 || len(f.sends) != 2 {
		t.Fatal("upload retry forked or reuploaded history")
	}

}
