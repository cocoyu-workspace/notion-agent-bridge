package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// First-turn images are staged for the user; follow-up images belong to the
// already selected thread. Notion cannot resolve staged files in a follow-up.
// Credentials are sent only to Notion; the signed PUT uses only supplied headers.
func uploadAgentImage(acc *Account, file *FileAttachment) (*UploadedAttachment, error) {
	return uploadAgentImageWithClients(acc, file, getChromeHTTPClient(30*time.Second), &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
}

func uploadAgentImageWithClients(acc *Account, file *FileAttachment, api, storage *http.Client) (*UploadedAttachment, error) {
	return uploadAgentImageForThreadWithClients(context.Background(), acc, file, "", api, storage)
}

func uploadAgentImageForThreadWithClients(parent context.Context, acc *Account, file *FileAttachment, threadID string, api, storage *http.Client) (*UploadedAttachment, error) {
	if file == nil || len(file.Data) == 0 {
		return nil, fmt.Errorf("image bytes are required")
	}
	switch file.ContentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, fmt.Errorf("Agent uploads currently support PNG, JPEG, GIF and WebP images only")
	}
	return uploadAgentServiceFile(parent, acc, file, threadID, api, storage)
}

// uploadAgentServiceFile uploads bytes to the Agent service (image or text,
// e.g. an offloaded tool output) and returns the file reference.
func uploadAgentServiceFile(parent context.Context, acc *Account, file *FileAttachment, threadID string, api, storage *http.Client) (*UploadedAttachment, error) {
	if file == nil || len(file.Data) == 0 {
		return nil, fmt.Errorf("file bytes are required")
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 && parent.Err() != nil {
			return nil, lastErr
		}
		uploaded, transient, err := uploadAgentServiceFileOnce(parent, acc, file, threadID, api, storage)
		if err == nil {
			return uploaded, nil
		}
		lastErr = err
		if !transient || parent.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func uploadAgentServiceFileOnce(parent context.Context, acc *Account, file *FileAttachment, threadID string, api, storage *http.Client) (*UploadedAttachment, bool, error) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	post := func(endpoint string, body, output interface{}) (bool, error) {
		data, err := json.Marshal(body)
		if err != nil {
			return false, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(NotionAPIBase, "/")+endpoint, bytes.NewReader(data))
		if err != nil {
			return false, err
		}
		setNotionHeadersJSON(req, acc)
		resp, err := api.Do(req)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				RebuildChromeTransport()
			}
			return true, fmt.Errorf("Agent file API request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
			return transient, fmt.Errorf("Agent file %s HTTP %d", endpoint, resp.StatusCode)
		}
		reader, cleanup, err := decompressBody(resp)
		if err != nil {
			return true, err
		}
		if cleanup != nil {
			defer cleanup()
		}
		if err := json.NewDecoder(io.LimitReader(reader, 1<<20)).Decode(output); err != nil {
			return true, fmt.Errorf("invalid Agent file response")
		}
		return false, nil
	}
	target := map[string]string{"type": "user"}
	if threadID != "" {
		target = map[string]string{"type": "thread", "threadId": threadID}
	}
	var created struct {
		Type string `json:"type"`
		File struct {
			ID string `json:"id"`
		} `json:"file"`
		Upload struct {
			Type    string            `json:"type"`
			Method  string            `json:"method"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"upload"`
	}
	if transient, err := post("/createAgentServiceFileUploadURL", map[string]interface{}{
		"spaceId": acc.SpaceID, "target": target, "filename": file.FileName,
		"mediaType": file.ContentType, "sizeBytes": len(file.Data),
	}, &created); err != nil {
		return nil, transient, err
	}
	u, err := url.Parse(created.Upload.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		created.Type != "file_upload_url" || created.File.ID == "" || created.Upload.Type != "single_part" || created.Upload.Method != "PUT" {
		return nil, false, fmt.Errorf("unsupported Agent signed upload response")
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", u.String(), bytes.NewReader(file.Data))
	if err != nil {
		return nil, false, fmt.Errorf("invalid Agent signed upload request")
	}
	for name, value := range created.Upload.Headers {
		if strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Authorization") {
			return nil, false, fmt.Errorf("unexpected authentication header in signed upload")
		}
		req.Header.Set(name, value)
	}
	resp, err := storage.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			// Exclude the credential-bearing signed URL from diagnostics.
			cause := urlErr.Err
			for errors.As(cause, &urlErr) {
				cause = urlErr.Err
			}
			return nil, true, fmt.Errorf("Agent signed upload failed: %v", cause)
		}
		return nil, true, fmt.Errorf("Agent signed upload failed (%T)", err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, transient, fmt.Errorf("Agent signed upload HTTP %d", resp.StatusCode)
	}
	var completed struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if transient, err := post("/completeAgentServiceFileUpload", map[string]interface{}{
		"spaceId": acc.SpaceID, "target": target, "fileId": created.File.ID,
	}, &completed); err != nil {
		return nil, transient, err
	}
	if completed.Type != "file" || completed.ID != created.File.ID {
		return nil, false, fmt.Errorf("Agent file completion identity mismatch")
	}
	return &UploadedAttachment{AgentFileID: completed.ID, FileName: file.FileName, ContentType: file.ContentType, FileSizeBytes: int64(len(file.Data))}, false, nil
}
