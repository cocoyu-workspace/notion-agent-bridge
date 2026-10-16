package proxy

import (
	"errors"
	"fmt"
	"net/http"
)

// Upstream error classification shared by the router, the Agent transport and
// the quota check. Messages carry fixed text and short machine names only: no
// tokens, emails, space/thread IDs or upstream message text.

// crossCellErrorName is the error name Notion returns (HTTP 500) when the API
// domain in use is not routed correctly for the account's cell.
const crossCellErrorName = "MemcachedCrossCellError"

// crossCellHint is the operator-facing explanation. The bridge never switches
// domains by itself: the recommended default is a deployment choice.
const crossCellHint = "the configured proxy.notion_api_base is not routed correctly for this account's Notion cell. " +
	"Set proxy.notion_api_base to https://www.notion.so/api/v3 (or https://app.notion.com/api/v3, depending on the account) and restart. " +
	"The bridge did not switch domains and did not resend the request."

// crossCell reports Notion's routing-stage rejection of the API domain.
func (e *agentHTTPError) crossCell() bool {
	return e != nil && e.Status >= 500 && e.Code == crossCellErrorName
}

// paymentRequired reports an explicit upstream HTTP 402. It is a definitive
// rejection (definitiveRejection is true for it): the request was not accepted.
func (e *agentHTTPError) paymentRequired() bool {
	return e != nil && e.Status == http.StatusPaymentRequired
}

// quotaExhaustedFailure is what an Anthropic client sees when the Notion AI
// allowance is gone: HTTP 402 billing_error, x-should-retry: false (written by
// writeAgentFailureJSON because ShouldRetry is false). It is deliberately not a
// 5xx/overloaded error, which clients retry with backoff.
func quotaExhaustedFailure() agentFailure {
	return agentFailure{Kind: "quota_exhausted", Status: http.StatusPaymentRequired, ErrType: "billing_error",
		Message: "[bridge:quota_exhausted] the Notion AI usage allowance is exhausted for every available account. " +
			"This is not a temporary server fault and retrying will not help: add or switch to another Notion account, or wait for the allowance to reset."}
}

// crossCellFailure reports the routing error as a configuration problem. It is
// not retryable as-is (the same domain fails the same way).
func crossCellFailure(endpoint string) agentFailure {
	msg := "[bridge:notion_cross_cell] Notion rejected the request while routing it (HTTP 500, name=" + crossCellErrorName + "): " + crossCellHint
	if endpoint != "" {
		msg += " (endpoint=" + endpoint + ")"
	}
	return agentFailure{Kind: "notion_cross_cell", Status: http.StatusBadGateway, ErrType: "api_error", Message: msg}
}

// classifyUpstreamConfig recognizes the explicit upstream conditions that must
// win over the generic transient/pending classification. ok is false otherwise.
func classifyUpstreamConfig(err error) (agentFailure, bool) {
	var httpErr *agentHTTPError
	switch {
	case errors.Is(err, ErrQuotaExhausted):
		return quotaExhaustedFailure(), true
	case errors.As(err, &httpErr) && httpErr.paymentRequired():
		return quotaExhaustedFailure(), true
	case errors.As(err, &httpErr) && httpErr.crossCell():
		return crossCellFailure(httpErr.Endpoint), true
	}
	return agentFailure{}, false
}

// quotaCrossCellError returns an actionable error when a read-only quota call
// hit the routing error, or nil for any other response. The upstream body is
// not included. Read-only calls are not retried on another domain.
func quotaCrossCellError(version string, status int, body []byte) error {
	if status < 500 || agentErrorCodeFromBody(body) != crossCellErrorName {
		return nil
	}
	return fmt.Errorf("[bridge:notion_cross_cell] quota %s API HTTP %d (name=%s): %s", version, status, crossCellErrorName, crossCellHint)
}
