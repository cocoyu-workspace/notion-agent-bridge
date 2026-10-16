package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
)

// agentPost shares authenticated, bounded, credential-free protocol errors.
func agentPost(ctx context.Context, client *http.Client, acc *Account, endpoint string, body interface{}, result interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(NotionAPIBase, "/")+"/"+endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	setNotionHeadersJSON(req, acc)
	resp, err := client.Do(req)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			RebuildChromeTransport()
		}
		return fmt.Errorf("Notion Agent %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		code, keys := agentErrorCodeAndKeys(resp)
		herr := &agentHTTPError{Endpoint: endpoint, Status: resp.StatusCode, Code: code, BodyKeys: keys}
		if endpoint == "sendEventToAgentThread" || endpoint == "createAgentThread" {
			herr.Shape = describeAgentRequestShape(endpoint, data).String()
			log.Printf("[agent-thread] upstream rejected/failed %s HTTP %d code=%s error_keys=[%s] request_shape: %s", endpoint, resp.StatusCode, code, keys, herr.Shape)
			if herr.crossCell() {
				log.Printf("[bridge:notion_cross_cell] %s: %s", endpoint, crossCellHint)
			}
		}
		return herr
	}
	reader, cleanup, err := decompressBody(resp)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	if result == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(reader, 8<<20))
		return err
	}
	if err = json.NewDecoder(io.LimitReader(reader, 8<<20)).Decode(result); err != nil {
		var netErr net.Error
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr) || ctx.Err() != nil {
			return fmt.Errorf("Notion Agent %s: %w", endpoint, err) // body cut off: transient
		}
		return &agentInvalidResponseError{Endpoint: endpoint}
	}
	return nil
}
