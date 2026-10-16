package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// Recognising Claude Code's summarisation request (issue #13).
//
// Evidence (real Claude Code CLI, local fake-upstream probes; issue #13 and the
// 2.1.296 repro of this change):
//   - the request's last user message is the full summary instruction, sent as a
//     plain string; the text is byte-identical in 2.1.280 and 2.1.296 (6369
//     bytes, SHA-256 63e2bfb4...), so both versions share one fingerprint;
//   - it carries the session's client tools (8 tools in the 2.1.296 repro), so the
//     tool count says nothing;
//   - an ordinary follow-up has the same shape (history, assistant reply, one
//     more user message), so "a string user message" is no signal either.
//
// The signal is therefore the instruction itself, matched byte for byte against
// a versioned list of fingerprints (length + SHA-256; the text is not stored in
// the binary), on top of the conversation-prefix and completed-state checks in
// beginKeyed and, later, the summary hash (agent_compaction.go).
//
// Why the whole text and not only its stable head/tail. The first line
// ("CRITICAL: Respond with TEXT ONLY. ...") and the closing reminder are the
// parts a client is least likely to change, and matching only them would
// survive a rewording of the middle. They are used here ONLY to detect that a
// message looks like a summarisation instruction (see summarySuspected), never
// to serve or re-base it, because: (a) the same head/tail frame other
// summarisation prompts (partial/"up to here" compaction) whose continuation
// may have a different structure, so a head/tail
// match would not prove the re-base layout this bridge verified; (b) a reworded
// middle is exactly the case where nobody has seen the continuation layout
// that follows; (c) a head/tail match cannot be told apart from a user who
// pasted the beginning and end of the prompt. A client change therefore stops
// /compact until a fingerprint is added (a one-line change plus the testdata
// file), and the failure is explicit, early (at the summarisation request, with
// nothing sent) and names the known versions. Ordinary turns are unaffected.
//
// Scope: a message that LOOKS like a summarisation instruction but matches no
// listed version is refused with an explicit error; it is never sent to the
// bound thread and never forked. A message that merely quotes the instruction
// inside other text is an ordinary message.
//
// /compact <focus>. Claude Code 2.1.296 builds the instruction of "/compact
// focus on X" as the known text with "Additional Instructions:\n<focus>\n\n"
// inserted immediately before the closing "REMINDER: ..." paragraph (captured
// real text: testdata/claude-code-2.1.296-summary-instruction-focus.txt; the
// client source appends "\nAdditional Instructions:\n" + focus when the focus is
// non-blank). That form is accepted, strictly: the bytes before the insertion
// point and the bytes from the REMINDER paragraph on are each fingerprinted
// separately (length + SHA-256, no text stored), the middle must be exactly the
// marker, a non-blank focus and a blank line, and the focus is bounded
// (maxSummaryFocusBytes, valid UTF-8, no control characters other than
// newline/tab/CR). Any other change (edited prefix or suffix, text before or
// after, empty focus, other insertion point, blocks instead of a string, other than the auto-compact appended-block layout) stays a
// look-alike and is refused. The continuation request has the same layout as
// without a focus (the client builds it from the summary only; the focus shows up
// solely as <command-args>), so the same verification applies.
//
// A fingerprint is NOT an identity: the text is public and can be typed by a
// user. It narrows what is treated as a summarisation request; the summary hash,
// single use and the exact continuation structure remain the protection.

type summaryInstructionVersion struct {
	Versions []string // client versions observed to send exactly this text
	Source   string
	Length   int
	SHA256   string
	// The same text split before the closing "REMINDER" paragraph, for the
	// /compact <focus> form: bytes [0,PrefixLen) and [Length-SuffixLen,Length).
	PrefixLen    int
	PrefixSHA256 string
	SuffixLen    int
	SuffixSHA256 string
}

const (
	summaryFocusMarker = "Additional Instructions:\n"
	summaryFocusEnd    = "\n\n"
	// maxSummaryFocusBytes bounds the focus a user can type after /compact. A
	// focus is a short steering sentence; the bound keeps a pasted document from
	// being carried inside a "summarisation instruction".
	maxSummaryFocusBytes = 4096
)

var knownSummaryInstructions = []summaryInstructionVersion{
	{
		Versions: []string{"claude-code-2.1.280", "claude-code-2.1.296"},
		Source:   "issue #13 real-CLI probes (2.1.280) and the 2.1.296 repro; byte-identical text, testdata/claude-code-2.1.{280,296}-summary-instruction.txt",
		Length:   6369,
		SHA256:   "63e2bfb4ded280d5057a66d8a034ef352a73960d97d955b990e78f04f0db1fb0",

		PrefixLen: 6197, PrefixSHA256: "b68e05484a12302641367790df8f7b135f7f023de8dde4add8048358cc0d57fa",
		SuffixLen: 172, SuffixSHA256: "122adb2ab98f6a34684cfaea52a6300564da86ecf169583e56535ee5bebf427d",
	},
}

const (
	summaryInstructionHead = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools."
	summaryInstructionTail = "REMINDER: Do NOT call any tools. Respond with plain text only — an <analysis> block followed by a <summary> block. Tool calls will be rejected and you will fail the task."
)

type summaryKind int

const (
	summaryNone      summaryKind = iota // not a summarisation instruction
	summaryKnown                        // byte-identical to a listed version
	summarySuspected                    // looks like one, matches no listed version
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// validSummaryFocus: non-blank, bounded, valid UTF-8, no control characters
// except newline, carriage return and tab.
func validSummaryFocus(focus string) bool {
	if strings.TrimSpace(focus) == "" || len(focus) > maxSummaryFocusBytes || !utf8.ValidString(focus) {
		return false
	}
	for _, r := range focus {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0x7f {
			return false
		}
	}
	return true
}

// matchSummaryFocus reports whether content is a listed instruction with a
// /compact focus inserted the way the client does it.
func matchSummaryFocus(content string, v summaryInstructionVersion) bool {
	if v.PrefixLen == 0 || v.SuffixLen == 0 || len(content) < v.PrefixLen+len(summaryFocusMarker)+1+len(summaryFocusEnd)+v.SuffixLen {
		return false
	}
	middle := content[v.PrefixLen : len(content)-v.SuffixLen]
	if !strings.HasPrefix(middle, summaryFocusMarker) || !strings.HasSuffix(middle, summaryFocusEnd) || len(middle) < len(summaryFocusMarker)+len(summaryFocusEnd) {
		return false
	}
	focus := middle[len(summaryFocusMarker) : len(middle)-len(summaryFocusEnd)]
	return validSummaryFocus(focus) &&
		sha256Hex(content[:v.PrefixLen]) == v.PrefixSHA256 && sha256Hex(content[len(content)-v.SuffixLen:]) == v.SuffixSHA256
}

// matchKnownInstruction reports whether text is a listed instruction, exactly or
// with a /compact focus inserted the way the client does it.
func matchKnownInstruction(text string) (version string, ok bool) {
	digest := ""
	for _, v := range knownSummaryInstructions {
		if len(text) == v.Length {
			if digest == "" {
				digest = sha256Hex(text)
			}
			if digest == v.SHA256 {
				return strings.Join(v.Versions, "/"), true
			}
		}
		if matchSummaryFocus(text, v) {
			return strings.Join(v.Versions, "/") + " with /compact focus", true
		}
	}
	return "", false
}

// classifySummaryInstruction inspects the last user message of a request. A
// known instruction must arrive in one of the two observed layouts:
//   - /compact: the whole message is the instruction, sent as a plain string;
//   - auto-compact (2.1.296): the instruction is its OWN text block appended to
//     the pending user message (the compaction runs before that turn is sent, so
//     no assistant reply follows it); the blocks before it hold the pending
//     message, which must not be blank and must not itself look like an
//     instruction.
//
// Anything else that looks like an instruction (a lone instruction block, text
// after it, an edited text) is suspected and refused.
func classifySummaryInstruction(m ChatMessage) (summaryKind, string) {
	if m.Role != "user" {
		return summaryNone, ""
	}
	if m.StringContent {
		if v, ok := matchKnownInstruction(m.Content); ok {
			return summaryKnown, v
		}
	} else if m.TextBlocks >= 2 && m.LastTextBlock != "" && strings.HasSuffix(m.Content, m.LastTextBlock) {
		if v, ok := matchKnownInstruction(m.LastTextBlock); ok {
			pending := m.Content[:len(m.Content)-len(m.LastTextBlock)]
			if strings.TrimSpace(pending) != "" && !strings.Contains(pending, summaryInstructionHead) {
				return summaryKnown, v + ", appended to the pending user message (auto-compact)"
			}
		}
	} else if m.AfterToolResult && m.TextBlocks >= 1 && m.LastTextBlock != "" && strings.HasSuffix(m.Content, m.LastTextBlock) {
		if v, ok := matchKnownInstruction(m.LastTextBlock); ok {
			pending := m.Content[:len(m.Content)-len(m.LastTextBlock)]
			if normalizeSessionUserContent(pending) == "" || (strings.TrimSpace(pending) != "" && !strings.Contains(pending, summaryInstructionHead)) {
				return summaryKnown, v + ", appended to the tool_result user message"
			}
		}
	}
	if strings.HasPrefix(strings.TrimLeft(m.Content, " \t\r\n"), summaryInstructionHead) || strings.HasSuffix(strings.TrimRight(m.Content, " \t\r\n"), summaryInstructionTail) {
		return summarySuspected, ""
	}
	return summaryNone, ""
}

func isCompactSummarisationRequest(messages []ChatMessage) bool {
	ns := nonSystemMessages(messages)
	if len(ns) == 0 {
		return false
	}
	kind, _ := classifySummaryInstruction(ns[len(ns)-1])
	return kind == summaryKnown
}

func knownSummaryVersions() string {
	var versions []string
	for _, v := range knownSummaryInstructions {
		versions = append(versions, v.Versions...)
	}
	return strings.Join(versions, ", ")
}

func summaryInstructionNotRecognisedError() error {
	return &agentBindingError{Kind: "diverged", Msg: "this request looks like Claude Code's summarisation request (/compact or auto-compact) but its instruction does not match a version this bridge knows (" + knownSummaryVersions() + "; a /compact focus is accepted only in the exact form the client inserts it). It was not sent to the bound thread, no second chat was created and the binding is kept, so the session stays usable without compaction. To compact, use a supported client version or add the new instruction fingerprint to the bridge; otherwise start a new client session when the context is full"}
}
