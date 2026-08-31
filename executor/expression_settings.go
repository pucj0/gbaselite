package executor

import (
	"fmt"
	"strings"
)

const sessionLookupIdentifier = "\x00gbaselite_session"

func sessionFromLookup(lookup func(string) (any, error)) *Session {
	value, err := lookup(sessionLookupIdentifier)
	if err != nil {
		return nil
	}
	session, _ := value.(*Session)
	return session
}

func compareWithLookup(lookup func(string) (any, error), left, right any) int {
	if session := sessionFromLookup(lookup); session != nil {
		return session.Compare(left, right)
	}
	return compareAny(left, right)
}

func likeMatchWithLookup(lookup func(string) (any, error), value, pattern string) bool {
	session := sessionFromLookup(lookup)
	if session == nil || !strings.HasSuffix(strings.ToLower(session.CollationConnection), "_bin") && !strings.EqualFold(session.CollationConnection, "binary") {
		value = strings.ToLower(value)
		pattern = strings.ToLower(pattern)
	}
	return likeMatchCaseSensitive(value, pattern)
}

func sqlInEqualWithLookup(lookup func(string) (any, error), left, right any) (bool, bool, error) {
	leftRow, leftIsRow := left.([]any)
	rightRow, rightIsRow := right.([]any)
	if leftIsRow || rightIsRow {
		if !leftIsRow || !rightIsRow || len(leftRow) != len(rightRow) {
			return false, false, fmt.Errorf("IN row column count mismatch")
		}
		unknown := false
		for index := range leftRow {
			equal, itemUnknown, err := sqlInEqualWithLookup(lookup, leftRow[index], rightRow[index])
			if err != nil {
				return false, false, err
			}
			if !equal && !itemUnknown {
				return false, false, nil
			}
			unknown = unknown || itemUnknown
		}
		return !unknown, unknown, nil
	}
	if left == nil || right == nil {
		return false, true, nil
	}
	return compareWithLookup(lookup, left, right) == 0, false, nil
}
