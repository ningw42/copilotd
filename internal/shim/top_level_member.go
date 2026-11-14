package shim

import (
	"bytes"
	"encoding/json/jsontext"
	"io"
	"unicode/utf8"
)

// topLevelMember is the evidence one scan gathers about one member name of a
// payload's top-level JSON object: how often the name occurred, and its last
// value exactly as written. Set name before scanning.
//
// Evidence describes only the bytes that were scanned. Each Shim scans the
// payload it is handed and keeps the result to itself: an earlier layer may
// have rewritten those bytes, so evidence is never passed between layers.
type topLevelMember struct {
	name        string
	occurrences int
	// raw is the last occurrence's value, aliasing the scanned payload at
	// [start, end).
	raw        []byte
	start, end int
}

// uniqueString returns the member's value only when it occurred exactly once,
// is a JSON string, and decodes losslessly. AppendUnquote rejects every other
// value kind as well as lone surrogate escapes, which would otherwise decode
// to a replacement character the sender never wrote.
func (m *topLevelMember) uniqueString() (string, bool) {
	if m.occurrences != 1 {
		return "", false
	}
	decoded, err := jsontext.AppendUnquote(nil, m.raw)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

// scanTopLevelMembers walks exactly one JSON object followed only by
// whitespace, gathering evidence for each requested member by its decoded,
// case-sensitive name. Unrelated values are validated but never decoded or
// re-encoded, and unrelated duplicate names are allowed. A name written with a
// lone surrogate escape decodes to a replacement character, so it is a
// different name from any requested one that lacks that character.
//
// It reports false, leaving every member without evidence, for any other
// payload. A payload that is not valid UTF-8 is malformed JSON text.
//
// Every scan starts from no evidence and keeps only the requested names, so
// the result depends on this payload alone even when a member is reused.
func scanTopLevelMembers(payload []byte, members ...*topLevelMember) bool {
	clearTopLevelEvidence(members)
	if walkTopLevelMembers(payload, members) {
		return true
	}
	clearTopLevelEvidence(members)
	return false
}

func clearTopLevelEvidence(members []*topLevelMember) {
	for _, member := range members {
		*member = topLevelMember{name: member.name}
	}
}

// walkTopLevelMembers is the traversal behind scanTopLevelMembers. It adds to
// whatever evidence the members hold, and on false it may leave evidence from
// the members it passed before giving up.
func walkTopLevelMembers(payload []byte, members []*topLevelMember) bool {
	if !utf8.Valid(payload) {
		return false
	}
	// Strings in unrelated members may carry lone surrogate escapes, which are
	// grammatical JSON; requested members are decoded strictly by uniqueString.
	decoder := jsontext.NewDecoder(bytes.NewReader(payload),
		jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if token, err := decoder.ReadToken(); err != nil || token.Kind() != '{' {
		return false
	}
	for decoder.PeekKind() != '}' {
		token, err := decoder.ReadToken()
		if err != nil {
			return false
		}
		// A Token is voided by the next read, so resolve its name first.
		name := token.String()
		value, err := decoder.ReadValue()
		if err != nil {
			return false
		}
		for _, member := range members {
			if member.name == name {
				member.occurrences++
				member.end = int(decoder.InputOffset())
				member.start = member.end - len(value)
				member.raw = payload[member.start:member.end]
			}
		}
	}
	if token, err := decoder.ReadToken(); err != nil || token.Kind() != '}' {
		return false
	}
	// The decoder accepts a stream of values; anything but end of input after
	// the object, including a second value, is trailing content.
	_, err := decoder.ReadToken()
	return err == io.EOF
}
