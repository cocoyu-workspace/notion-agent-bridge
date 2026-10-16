package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
)

// Claude Code sends this header only on requests made by a subagent. The
// subagent's metadata.user_id carries the same session_id as its parent, so the
// header is the only stable child identity (verified on Claude Code 2.1.280:
// absent on main requests, identical across one child's requests).
const anthropicAgentIDHeader = "X-Claude-Code-Agent-Id"

const maxAnthropicAgentIDLen = 128

// anthropicSubagentIDFromHeader reports the child agent ID. present is true when
// the header exists at all; id is non-empty only when every value is valid. A
// present but invalid header must never be treated as "main".
func anthropicSubagentIDFromHeader(h http.Header) (id string, present bool) {
	values := h.Values(anthropicAgentIDHeader)
	if len(values) == 0 {
		return "", false
	}
	if len(values) != 1 || !validAnthropicAgentID(values[0]) {
		return "", true
	}
	return values[0], true
}

func validAnthropicAgentID(id string) bool {
	if id == "" || len(id) > maxAnthropicAgentIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// anthropicClientSessionIDForRequest returns the identity that keys the Agent
// thread binding of one /v1/messages request:
//   - main request (no agent header): the client session, unchanged;
//   - subagent request: a derived key of parent session + child agent ID, so each
//     child owns one thread and a child never shares or forks the main thread;
//   - invalid agent header, or a child without a parent session: "" (no stable
//     binding; the request carries its full history on a fresh thread).
//
// Identity is never guessed from system prompts, tool counts or request IDs.
func anthropicClientSessionIDForRequest(h http.Header, metadata map[string]interface{}) string {
	parent := extractAnthropicClientSessionID(metadata)
	child, present := anthropicSubagentIDFromHeader(h)
	if !present {
		return parent
	}
	if child == "" || parent == "" {
		return ""
	}
	return subagentClientKey(parent, child)
}

// subagentClientKey length-prefixes both parts so (a, bc) and (ab, c) differ. The
// result embeds no raw ID; agentClientKey hashes it again before it is stored.
func subagentClientKey(parent, child string) string {
	sum := sha256.Sum256([]byte("subagent:" + strconv.Itoa(len(parent)) + ":" + parent + ":" + strconv.Itoa(len(child)) + ":" + child))
	return "subagent:" + hex.EncodeToString(sum[:])
}

// anthropicSessionSaltForRequest extends the in-memory session fingerprint salt
// with the child identity, so two subagents of one parent that start from an
// identical prompt are never treated as one conversation. Main requests keep the
// original salt.
func anthropicSessionSaltForRequest(h http.Header, metadata map[string]interface{}) string {
	salt := extractAnthropicSessionSalt(metadata)
	child, present := anthropicSubagentIDFromHeader(h)
	if !present {
		return salt
	}
	return salt + "|" + subagentClientKey(salt, child)
}

const (
	defaultSubagentModel  = "sonnet-5.5"
	defaultSubagentEffort = "high"
)

// applyAgentSubagentDefaults applies the local subagent defaults (sonnet-5.5 + high)
// when a request carries a valid X-Claude-Code-Agent-Id header. Claude Code's CLI
// --effort flag (default low) is process-wide and is otherwise inherited by subagent
// requests on the wire.
func applyAgentSubagentDefaults(h http.Header, model string, outCfg *AnthropicOutputConfig) (string, *AnthropicOutputConfig) {
	child, present := anthropicSubagentIDFromHeader(h)
	if !present || child == "" {
		return model, outCfg
	}
	stripped, askSuffix := StripAskModeSuffix(model)
	if stripped == "" || stripped == "auto" {
		model = defaultSubagentModel
		if askSuffix {
			model += "-ask"
		}
	} else if _, err := agentThreadModelID(stripped); err != nil {
		model = defaultSubagentModel
		if askSuffix {
			model += "-ask"
		}
	}
	if outCfg == nil {
		outCfg = &AnthropicOutputConfig{Effort: defaultSubagentEffort}
	} else {
		copied := *outCfg
		copied.Effort = defaultSubagentEffort
		outCfg = &copied
	}
	return model, outCfg
}
