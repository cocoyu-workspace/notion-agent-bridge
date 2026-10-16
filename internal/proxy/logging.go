package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

var debugLoggingEnabled atomic.Bool
var apiInputLoggingEnabled atomic.Bool
var apiOutputLoggingEnabled atomic.Bool
var notionRequestLoggingEnabled atomic.Bool
var notionResponseLoggingEnabled atomic.Bool
var globalLogWriter = newDebugFilterWriter()

var debugLogTags = []string{
	"[debug]",
	"[bridge]",
	"[session]",
	"[thinking]",
	"[req]",
	"[search]",
	"[upload-debug]",
}

type debugFilterWriter struct {
	mu   sync.RWMutex
	out  io.Writer
	file *os.File
}

func newDebugFilterWriter() *debugFilterWriter {
	return &debugFilterWriter{
		out: os.Stderr,
	}
}

func init() {
	debugLoggingEnabled.Store(true)
	log.SetOutput(globalLogWriter)
}

func (w *debugFilterWriter) Write(p []byte) (int, error) {
	if !DebugLoggingEnabled() && isDebugLogLine(p) {
		return len(p), nil
	}
	w.mu.RLock()
	out := w.out
	w.mu.RUnlock()
	if out == nil {
		out = os.Stderr
	}
	return out.Write(p)
}

func (w *debugFilterWriter) SetOutputPath(path string) error {
	path = strings.TrimSpace(path)

	target := io.Writer(os.Stderr)
	var file *os.File

	if path != "" {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return err
			}
		}

		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return err
		}
		target = f
		file = f
	}

	w.mu.Lock()
	oldFile := w.file
	w.out = target
	w.file = file
	w.mu.Unlock()

	if oldFile != nil && oldFile != file {
		_ = oldFile.Close()
	}
	return nil
}

func isDebugLogLine(p []byte) bool {
	line := string(p)
	for _, tag := range debugLogTags {
		if strings.Contains(line, tag) {
			return true
		}
	}
	return false
}

func SetDebugLoggingEnabled(enabled bool) {
	debugLoggingEnabled.Store(enabled)
}

func DebugLoggingEnabled() bool {
	return debugLoggingEnabled.Load()
}

func ConfigureLogOutput(path string) error {
	return globalLogWriter.SetOutputPath(path)
}

func SetAPILogInputEnabled(enabled bool) {
	apiInputLoggingEnabled.Store(enabled)
}

func APILogInputEnabled() bool {
	return apiInputLoggingEnabled.Load()
}

func SetAPILogOutputEnabled(enabled bool) {
	apiOutputLoggingEnabled.Store(enabled)
}

func APILogOutputEnabled() bool {
	return apiOutputLoggingEnabled.Load()
}

func SetNotionRequestLoggingEnabled(enabled bool) {
	notionRequestLoggingEnabled.Store(enabled)
}

func NotionRequestLoggingEnabled() bool {
	return notionRequestLoggingEnabled.Load()
}

func SetNotionResponseLoggingEnabled(enabled bool) {
	notionResponseLoggingEnabled.Store(enabled)
}

func NotionResponseLoggingEnabled() bool {
	return notionResponseLoggingEnabled.Load()
}

func LogAPIInputJSON(requestID, label string, v interface{}) {
	if !APILogInputEnabled() {
		return
	}
	logJSONPayload("[api-in]", requestID, label, v)
}

func LogAPIInputJSONBytes(requestID, label string, raw []byte) {
	if !APILogInputEnabled() {
		return
	}
	logJSONBytesPayload("[api-in]", requestID, label, raw)
}

func LogAPIInputText(requestID, label, text string) {
	if !APILogInputEnabled() {
		return
	}
	logTextPayload("[api-in]", requestID, label, text)
}

func LogAPIOutputJSON(requestID, label string, v interface{}) {
	if !APILogOutputEnabled() {
		return
	}
	logJSONPayload("[api-out]", requestID, label, v)
}

func LogAPIOutputText(requestID, label, text string) {
	if !APILogOutputEnabled() {
		return
	}
	logTextPayload("[api-out]", requestID, label, text)
}

func LogNotionRequestJSON(requestID, label string, v interface{}) {
	if !NotionRequestLoggingEnabled() {
		return
	}
	logJSONPayload("[notion-req]", requestID, label, v)
}

func LogNotionResponseJSON(requestID, label string, v interface{}) {
	if !NotionResponseLoggingEnabled() {
		return
	}
	logJSONPayload("[notion-resp]", requestID, label, v)
}

func LogNotionResponseJSONBytes(requestID, label string, raw []byte) {
	if !NotionResponseLoggingEnabled() {
		return
	}
	logJSONBytesPayload("[notion-resp]", requestID, label, raw)
}

func LogNotionResponseText(requestID, label, text string) {
	if !NotionResponseLoggingEnabled() {
		return
	}
	logTextPayload("[notion-resp]", requestID, label, text)
}

func logJSONPayload(tag, requestID, label string, v interface{}) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Printf("%s %s %s marshal error: %v", tag, requestID, label, err)
		return
	}
	logTextPayload(tag, requestID, label, string(raw))
}

func logJSONBytesPayload(tag, requestID, label string, raw []byte) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err == nil {
		logTextPayload(tag, requestID, label, buf.String())
		return
	}
	logTextPayload(tag, requestID, label, string(raw))
}

func logTextPayload(tag, requestID, label, text string) {
	label = strings.TrimSpace(label)
	requestID = strings.TrimSpace(requestID)
	if requestID != "" && label != "" {
		log.Printf("%s %s %s\n%s", tag, requestID, label, text)
		return
	}
	if requestID != "" {
		log.Printf("%s %s\n%s", tag, requestID, text)
		return
	}
	if label != "" {
		log.Printf("%s %s\n%s", tag, label, text)
		return
	}
	log.Printf("%s\n%s", tag, text)
}
