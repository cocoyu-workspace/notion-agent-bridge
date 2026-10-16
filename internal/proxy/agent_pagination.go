package proxy

import (
	"errors"
	"fmt"
)

// Bounded paging over the real getThreadTranscript cursor chain (issue #17).
//
// Wire contract (frontend request fields and response fields, confirmed
// read-only against a real Business account by Codex; not guessed): request
// {spaceId, threadId, direction:"backward", cursor?, limit}; response
// {has_more_backward, backward_cursor, patches, session}. The newest page is
// requested without a cursor; while has_more_backward is true the response's
// backward_cursor (an opaque string, never logged) fetches the next, strictly
// older page. limit is NOT an entity/patch cap (limit 2 returned 52 patches,
// limit 100 returned 762), so the bridge keeps limit=100 and always follows the
// chain instead of assuming a larger limit reads a whole thread. A cursor page
// can fail with a message error (observed on a third page): that is an
// integrity error, never "the thread ended".

var (
	// agentTranscriptMaxPages bounds the pages read per poll; agentTranscriptMaxPatches
	// bounds the merged patches. Exceeding either is a refusal, never a partial read.
	agentTranscriptMaxPages   = 32
	agentTranscriptMaxPatches = 20000
)

// agentInvalidResponseError: the service answered 200 with a body that is not
// the expected JSON (for example an HTML error page).
type agentInvalidResponseError struct{ Endpoint string }

func (e *agentInvalidResponseError) Error() string {
	return fmt.Sprintf("Notion Agent invalid response for %s", e.Endpoint)
}

// agentTranscriptFetch reads one transcript page. cursor "" requests the newest
// page; otherwise it is the previous page's backward_cursor.
type agentTranscriptFetch func(cursor string) (agentTranscriptPage, error)

func incompleteErr(format string, args ...interface{}) error {
	return &agentTranscriptIncompleteError{msg: fmt.Sprintf(format, args...)}
}

// collectAgentTranscript follows backward_cursor from the newest page until the
// thread is complete (!has_more_backward), an entity at/below baseline is
// covered, or stop(merged) reports it has seen enough. Older pages are placed
// first so the newest version of an entity wins when the patches are applied in
// order. Every page after the first is checked: it must be a transcript page,
// its cursor must not repeat, and its entities must be strictly older than the
// previous page's. Running out of pages/patches/cursor, or any unreadable cursor
// page, yields agentTranscriptIncompleteError; transient failures (network,
// deadline, 5xx, cancel) are returned unchanged so the caller can keep the turn
// pending. The newest page's session is kept: older pages never decide state.
func collectAgentTranscript(fetch agentTranscriptFetch, baseline agentSeq, stop func(agentTranscriptPage) bool) (agentTranscriptPage, error) {
	var merged agentTranscriptPage
	cursor := ""
	seen := map[string]bool{}
	var prevMin agentSeq
	for n := 0; n < agentTranscriptMaxPages; n++ {
		page, err := fetch(cursor)
		if err != nil {
			if n == 0 {
				return agentTranscriptPage{}, err
			}
			return agentTranscriptPage{}, cursorPageError(n+1, err)
		}
		if n == 0 {
			merged = page
			if page.Type != agentTranscriptPageType {
				// Unreadable newest page (for example HTTP 200 {"message":...}).
				// It says nothing about the remote turn: not "empty", not "failed",
				// not "never submitted". Callers keep the binding and re-poll.
				return agentTranscriptPage{}, transcriptErrKind("transcript_format", "unexpected Notion Agent transcript type")
			}
			prevMin = minPutSequence(page)
		} else {
			if page.Type != agentTranscriptPageType {
				return agentTranscriptPage{}, incompleteErr("Notion Agent transcript page %d is not a transcript page; refusing incomplete output", n+1)
			}
			curMin := minPutSequence(page)
			if (page.HasMoreBackward && curMin <= 0) || (curMin > 0 && prevMin > 0 && curMin >= prevMin) {
				return agentTranscriptPage{}, incompleteErr("Notion Agent transcript page %d does not reach earlier entities; refusing incomplete output", n+1)
			}
			if curMin > 0 {
				prevMin = curMin
			}
			patches := make([]agentTranscriptPatch, 0, len(page.Patches)+len(merged.Patches))
			patches = append(append(patches, page.Patches...), merged.Patches...)
			merged.Patches = patches
			merged.HasMoreBackward = page.HasMoreBackward
			merged.BackwardCursor = page.BackwardCursor
		}
		if len(merged.Patches) > agentTranscriptMaxPatches {
			return agentTranscriptPage{}, incompleteErr("Notion Agent transcript exceeds the bounded patch count; refusing incomplete output")
		}
		if !merged.HasMoreBackward || pageCoversBaseline(merged, baseline) {
			merged.HasMoreBackward = false
			merged.BackwardCursor = ""
			return merged, nil
		}
		if stop != nil && stop(merged) {
			return merged, nil
		}
		next := page.BackwardCursor
		seen[cursor] = true
		if next == "" {
			return agentTranscriptPage{}, incompleteErr("Notion Agent transcript has more pages but no cursor to continue; refusing incomplete output")
		}
		if seen[next] {
			return agentTranscriptPage{}, incompleteErr("Notion Agent transcript cursor repeated; refusing incomplete output")
		}
		cursor = next
	}
	return agentTranscriptPage{}, incompleteErr("Notion Agent transcript exceeds the bounded page count (%d); refusing incomplete output", agentTranscriptMaxPages)
}

// cursorPageError classifies a failed read of page number n (>=2). A definitive
// rejection or an unparsable body means the chain cannot be completed: that is a
// completeness error, and must not be mistaken for "the thread is gone". Anything
// else (network, deadline, 5xx, client gone) stays transient.
func cursorPageError(n int, err error) error {
	var httpErr *agentHTTPError
	var invalid *agentInvalidResponseError
	if (errors.As(err, &httpErr) && httpErr.definitiveRejection()) || errors.As(err, &invalid) {
		return incompleteErr("Notion Agent transcript page %d could not be read (%s); refusing incomplete output", n, err)
	}
	return err
}

// minPutSequence is the lowest positive entity sequence among the page's puts.
func minPutSequence(page agentTranscriptPage) agentSeq {
	var low agentSeq
	for _, patch := range page.Patches {
		if patch.Op == "put" && patch.Entity.Sequence > 0 && (low == 0 || patch.Entity.Sequence < low) {
			low = patch.Entity.Sequence
		}
	}
	return low
}

// pageCoversBaseline reports whether an entity at/below baseline survives the
// patch sequence, i.e. everything newer than the previous turn has been read.
func pageCoversBaseline(page agentTranscriptPage, baseline agentSeq) bool {
	if baseline <= 0 {
		return false
	}
	alive := map[string]agentSeq{}
	for _, patch := range page.Patches {
		switch patch.Op {
		case "put":
			alive[patch.Entity.ID] = patch.Entity.Sequence
		case "remove":
			delete(alive, patch.ID)
		}
	}
	for _, seq := range alive {
		if seq > 0 && seq <= baseline {
			return true
		}
	}
	return false
}
