package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
)

var errInvalidMonthlyLimit = errors.New("invalid monthly_limit_micro")

// parseMonthlyLimit preserves the three wire states: absent, explicit null
// (unlimited), and a non-negative decimal string in micro-yuan.
func parseMonthlyLimit(raw json.RawMessage) (*int64, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false, nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, true, nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
		return nil, true, errInvalidMonthlyLimit
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 || strconv.FormatInt(value, 10) != text {
		return nil, true, errInvalidMonthlyLimit
	}
	return &value, true, nil
}
