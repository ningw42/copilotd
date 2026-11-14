package shim

import (
	"strings"
	"testing"
)

func TestScanTopLevelMembersReportsOccurrencesSpansAndStrictStrings(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		occurrences   int
		// span is the raw value the reported byte span selects.
		span string
		// value and unique are the strict string decode of the member.
		value  string
		unique bool
	}{
		{name: "string", payload: `{"model":"A"}`, occurrences: 1, span: `"A"`, value: "A", unique: true},
		{name: "empty string", payload: `{"model":""}`, occurrences: 1, span: `""`, value: "", unique: true},
		{name: "span excludes surrounding whitespace", payload: "\n { \"model\"\t:\t\"A\"\t,\n\"x\" : 1 } \n", occurrences: 1, span: `"A"`, value: "A", unique: true},
		{name: "span keeps the escaped spelling", payload: `{"model":"gpt-\u0073ol\/x"}`, occurrences: 1, span: `"gpt-\u0073ol\/x"`, value: "gpt-sol/x", unique: true},
		{name: "escaped name", payload: `{"mod\u0065l":"A"}`, occurrences: 1, span: `"A"`, value: "A", unique: true},
		{name: "valid surrogate pair", payload: `{"model":"\ud83d\ude80"}`, occurrences: 1, span: `"\ud83d\ude80"`, value: "\U0001F680", unique: true},
		{name: "escaped replacement character", payload: `{"model":"A\ufffd"}`, occurrences: 1, span: `"A\ufffd"`, value: "A\uFFFD", unique: true},
		{name: "unrelated duplicates and lone surrogates", payload: `{"x":1,"model":"A","x":"\ud800","y":{"model":"B","model":"C"}}`, occurrences: 1, span: `"A"`, value: "A", unique: true},
		{name: "unusual numbers in unrelated members", payload: `{"seed":123456789012345678901234567890,"budget":1.0E+400,"model":"A"}`, occurrences: 1, span: `"A"`, value: "A", unique: true},

		{name: "absent", payload: `{"input":"A"}`},
		{name: "empty object", payload: `{}`},
		{name: "nested only", payload: `{"input":{"model":"A"}}`},
		{name: "differently cased name", payload: `{"Model":"A","MODEL":"A"}`},
		{name: "name with a lone surrogate", payload: `{"model\ud800":"A"}`},

		{name: "duplicate reports the last span", payload: `{"model":"A","model":"B"}`, occurrences: 2, span: `"B"`},
		{name: "identical duplicate", payload: `{"model":"A","model":"A"}`, occurrences: 2, span: `"A"`},
		{name: "duplicate via escaped name", payload: `{"model":"A","mod\u0065l":"B"}`, occurrences: 2, span: `"B"`},
		{name: "duplicate with a wrong-typed twin", payload: `{"model":null,"model":"A"}`, occurrences: 2, span: `"A"`},

		{name: "null", payload: `{"model":null}`, occurrences: 1, span: `null`},
		{name: "number", payload: `{"model":42}`, occurrences: 1, span: `42`},
		{name: "boolean", payload: `{"model":true}`, occurrences: 1, span: `true`},
		{name: "array", payload: `{"model":["A"]}`, occurrences: 1, span: `["A"]`},
		{name: "object", payload: `{"model":{"id":"A"}}`, occurrences: 1, span: `{"id":"A"}`},
		{name: "lone high surrogate", payload: `{"model":"A\ud800"}`, occurrences: 1, span: `"A\ud800"`},
		{name: "lone low surrogate", payload: `{"model":"A\udc00"}`, occurrences: 1, span: `"A\udc00"`},
		{name: "reversed surrogate pair", payload: `{"model":"\ude80\ud83d"}`, occurrences: 1, span: `"\ude80\ud83d"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(tc.payload)
			member := topLevelMember{name: "model"}
			if !scanTopLevelMembers(payload, &member) {
				t.Fatal("scan declined one well-formed JSON object")
			}
			if member.occurrences != tc.occurrences {
				t.Errorf("occurrences = %d, want %d", member.occurrences, tc.occurrences)
			}
			if tc.occurrences > 0 {
				if got := string(payload[member.start:member.end]); got != tc.span {
					t.Errorf("span selects %q, want %q", got, tc.span)
				}
			}
			if value, unique := member.uniqueString(payload); value != tc.value || unique != tc.unique {
				t.Errorf("uniqueString = (%q, %v), want (%q, %v)", value, unique, tc.value, tc.unique)
			}
		})
	}
}

func TestScanTopLevelMembersGathersEachNameIndependently(t *testing.T) {
	payload := []byte(`{"type":"a","model":"A","type":"b","other":"C"}`)
	model := topLevelMember{name: "model"}
	eventType := topLevelMember{name: "type"}
	absent := topLevelMember{name: "missing"}
	if !scanTopLevelMembers(payload, &model, &eventType, &absent) {
		t.Fatal("scan declined one well-formed JSON object")
	}
	if value, unique := model.uniqueString(payload); value != "A" || !unique {
		t.Errorf("model = (%q, %v), want the unique string A", value, unique)
	}
	if got := string(payload[eventType.start:eventType.end]); eventType.occurrences != 2 || got != `"b"` {
		t.Errorf("type = %d occurrences ending at %q, want 2 ending at \"b\"", eventType.occurrences, got)
	}
	if _, unique := eventType.uniqueString(payload); unique {
		t.Error("duplicate type decoded as unique")
	}
	if _, unique := absent.uniqueString(payload); absent.occurrences != 0 || unique {
		t.Errorf("absent member = %+v, want no evidence", absent)
	}
}

func TestScanTopLevelMembersYieldsNoEvidenceUnlessPayloadIsOneObject(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{name: "empty", payload: ``},
		{name: "whitespace", payload: " \n"},
		{name: "array", payload: `[{"model":"A"}]`},
		{name: "string", payload: `"model"`},
		{name: "null", payload: `null`},
		{name: "truncated", payload: `{"model":"A"`},
		{name: "truncated after comma", payload: `{"model":"A",`},
		{name: "trailing comma", payload: `{"model":"A",}`},
		{name: "single-quoted string", payload: `{"model":'A'}`},
		{name: "unquoted name", payload: `{model:"A"}`},
		{name: "trailing text", payload: `{"model":"A"} x`},
		{name: "trailing second object", payload: `{"model":"A"}{"model":"A"}`},
		{name: "trailing close brace", payload: `{"model":"A"}}`},
		{name: "invalid UTF-8 in the member", payload: "{\"model\":\"A\xff\"}"},
		{name: "invalid UTF-8 elsewhere", payload: "{\"model\":\"A\",\"input\":\"\xff\"}"},
		// Go's JSON decoders stop at 10,000 levels of nesting.
		{name: "nesting beyond the decoder's depth limit", payload: `{"model":"A","x":` + strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(tc.payload)
			member := topLevelMember{name: "model"}
			if scanTopLevelMembers(payload, &member) {
				t.Fatal("scan accepted a payload that is not exactly one JSON object")
			}
			// A caller that ignored the verdict must still find nothing to act on.
			if value, unique := member.uniqueString(payload); member.occurrences != 0 || unique {
				t.Errorf("declined payload left evidence: %+v decoding to (%q, %v)", member, value, unique)
			}
		})
	}
}
