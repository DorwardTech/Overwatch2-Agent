package lasertag

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// lenientInt parses a JSON value that may be a number, a numeric string, or
// null (the scrape emits strings and blanks). Returns nil when not parseable.
func lenientInt(raw json.RawMessage) *int {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return &n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		if v, err := strconv.Atoi(s); err == nil {
			return &v
		}
	}
	return nil
}

// lenientString parses a JSON value that may be a string or null.
func lenientString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func sortByID(packs []PackLive) {
	sort.Slice(packs, func(i, j int) bool { return packs[i].PackID < packs[j].PackID })
}
