package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// agentSeq is a transcript sequence number. The real getThreadTranscript
// protocol is not integral: bookkeeping entities (for example kind "status")
// carry fractional sequences such as 26.5 that sort between two turns. It is
// therefore a float64 everywhere it is decoded, compared, sorted or persisted
// (thread baselines), never an int.
type agentSeq float64

// UnmarshalJSON accepts a JSON number (integer or fractional), a numeric string
// and null (treated as 0 = unknown). Anything else is an error rather than being
// silently dropped, so a protocol change is surfaced instead of hidden.
func (s *agentSeq) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*s = 0
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		v, err := strconv.ParseFloat(str, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("invalid transcript sequence %q", str)
		}
		*s = agentSeq(v)
		return nil
	}
	var v float64
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("invalid transcript sequence %s", string(data))
	}
	*s = agentSeq(v)
	return nil
}
