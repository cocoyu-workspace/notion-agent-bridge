package proxy

import (
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Notion Agent approval modes understood by the createAgentThread /
// sendEventToAgentThread policies.approval_mode field.
const (
	agentApprovalAsk         = "ask"
	agentApprovalReviewForMe = "review_for_me"
	agentApprovalYolo        = "yolo"
)

// Where the effective approval mode came from (reported to the launcher).
const (
	agentApprovalSourceDefault = "default"
	agentApprovalSourceConfig  = "config"
	agentApprovalSourceEnv     = "env"
	agentApprovalSourceUnknown = "unknown-fallback"
)

// normalizeAgentApprovalMode maps a configured value to a known mode. Empty
// input defaults to "yolo" (Notion's skip-all-permissions); unrecognised
// non-empty input never relaxes and yields "ask" with ok=false so callers can
// report it.
func normalizeAgentApprovalMode(raw string) (mode string, ok bool) {
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case "":
		return agentApprovalYolo, true
	case agentApprovalAsk, agentApprovalReviewForMe, agentApprovalYolo:
		return v, true
	default:
		return agentApprovalAsk, false
	}
}

// agentApprovalEffective returns the approval mode sent to Notion and where it
// came from. Priority: AGENT_APPROVAL_MODE env > proxy.agent_approval_mode > yolo.
// The env value is read here (not only at load) so tests and the launcher's
// session-scoped override behave the same as config loading.
func agentApprovalEffective() (mode, source string) {
	raw, source := "", agentApprovalSourceDefault
	if AppConfig != nil && strings.TrimSpace(AppConfig.Proxy.AgentApprovalMode) != "" {
		raw, source = AppConfig.Proxy.AgentApprovalMode, agentApprovalSourceConfig
	}
	if v := os.Getenv("AGENT_APPROVAL_MODE"); strings.TrimSpace(v) != "" {
		raw, source = v, agentApprovalSourceEnv
	}
	mode, ok := normalizeAgentApprovalMode(raw)
	if !ok {
		return agentApprovalAsk, agentApprovalSourceUnknown
	}
	return mode, source
}

var agentApprovalLogOnce sync.Map

// agentApprovalPolicies is the policies object of every Agent request body.
func agentApprovalPolicies() map[string]string {
	mode, source := agentApprovalEffective()
	if _, seen := agentApprovalLogOnce.LoadOrStore(mode+"/"+source, true); !seen {
		log.Printf("[agent-approval] approval_mode=%s source=%s", mode, source)
	}
	return map[string]string{"approval_mode": mode}
}

// setAgentApprovalHeaders lets the local launcher read the approval mode this
// proxy WILL SEND to Notion (its configured value). It is not Notion's
// server-side or workspace-effective mode: Notion may reject or demote it, and
// neither the browser preference nor that decision is readable here. It reports
// only the mode name and its source, never credentials.
func setAgentApprovalHeaders(w http.ResponseWriter) {
	mode, source := agentApprovalEffective()
	w.Header().Set("X-Agent-Approval-Sent-Mode", mode)
	w.Header().Set("X-Agent-Approval-Sent-Source", source)
}
