package rag

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

func user(s string) provider.Message {
	return provider.Message{Role: "user", Content: provider.TextContent(s)}
}
func assistant(s string) provider.Message {
	return provider.Message{Role: "assistant", Content: provider.TextContent(s)}
}

// TestAutoPolicySmallTalk is the table of the brief: what "auto" skips and
// what it must not.
func TestAutoPolicySmallTalk(t *testing.T) {
	cases := []struct {
		text string
		want string // "" = retrieval runs
	}{
		// Greetings, thanks, farewells.
		{"merhaba", obs.RAGSkipGreeting},
		{"Merhaba!", obs.RAGSkipGreeting},
		{"Teşekkürler!", obs.RAGSkipGreeting},
		{"TEŞEKKÜRLER", obs.RAGSkipGreeting},
		{"tesekkurler", obs.RAGSkipGreeting},
		{"thanks", obs.RAGSkipGreeting},
		{"Thank you!", obs.RAGSkipGreeting},
		{"thank you so much", obs.RAGSkipGreeting},
		{"İYİ GÜNLER", obs.RAGSkipGreeting},
		{"iyi günler", obs.RAGSkipGreeting},
		{"IYI GUNLER", obs.RAGSkipGreeting},
		{"ıyı günler", obs.RAGSkipGreeting},
		{"İYİ GÜNLER", obs.RAGSkipGreeting}, // decomposed İ
		{"sağ ol", obs.RAGSkipGreeting},
		{"SAĞOL :)", obs.RAGSkipGreeting},
		{"hello", obs.RAGSkipGreeting},
		{"Hi there", obs.RAGSkipGreeting},
		{"Good morning!", obs.RAGSkipGreeting},
		{"günaydın", obs.RAGSkipGreeting},
		{"merhaba, nasılsın?", obs.RAGSkipGreeting},
		{"hey, how are you?", obs.RAGSkipGreeting},
		{"tamam teşekkürler", obs.RAGSkipGreeting},
		{"meeerhaba", obs.RAGSkipGreeting},
		{"teşekkürlerrr", obs.RAGSkipGreeting},
		{"  thanks \n", obs.RAGSkipGreeting},
		// Acknowledgements.
		{"ok", obs.RAGSkipAck},
		{"OK.", obs.RAGSkipAck},
		{"tamam", obs.RAGSkipAck},
		{"Evet", obs.RAGSkipAck},
		{"olur", obs.RAGSkipAck},
		{"yes", obs.RAGSkipAck},
		{"got it", obs.RAGSkipAck},
		{"tamam anladım", obs.RAGSkipAck},
		// Questions and anything with substance stay retrieved.
		{"İade politikası nedir?", ""},
		{"iade politikası nedir", ""},
		{"merhaba, iade politikası nedir?", ""},
		{"Teşekkürler ama iade nasıl yapılır?", ""},
		{"thanks, what is the refund policy?", ""},
		{"ok but what about shipping", ""},
		{"how are you handling returns", ""},
		{"nasılsın diye sormuyorum, kargo ücreti ne kadar", ""},
		{"hello world program", ""},
		{"", ""},
		{"???", ""},
		{"👍", ""},
		{strings.Repeat("merhaba ", 40), ""}, // long: never small talk
	}
	for _, tc := range cases {
		got := SkipReason(store.RetrievalPolicyAuto, false, false, []provider.Message{user(tc.text)})
		if got != tc.want {
			t.Errorf("auto, %q: skip = %q, want %q", tc.text, got, tc.want)
		}
		// The same text never skips under "always".
		if got := SkipReason(store.RetrievalPolicyAlways, false, false, []provider.Message{user(tc.text)}); got != "" {
			t.Errorf("always, %q: skip = %q, want none", tc.text, got)
		}
	}
}

// TestRepeatedQuestionIsRetrieved: asking the same thing again is not small
// talk. The context of the first answer reached the upstream only, never the
// client's history, so a skipped retrieval would answer without it (ADR-024).
func TestRepeatedQuestionIsRetrieved(t *testing.T) {
	q := "İade politikası nedir?"
	for name, msgs := range map[string][]provider.Message{
		"same text":      {user(q), assistant("30 gün."), user(q)},
		"folded variant": {user(q), assistant("30 gün."), user("iade politikasi nedir")},
		"prefill after":  {user(q), assistant("30 gün."), user(q), assistant("prefill")},
	} {
		for _, policy := range []string{store.RetrievalPolicyAlways, store.RetrievalPolicyAuto} {
			if got := SkipReason(policy, false, false, msgs); got != "" {
				t.Errorf("%s under %s: skip = %q, want retrieval", name, policy, got)
			}
		}
	}
}

// TestSkipDecisionOrder pins the fixed order: policy_off, client_off,
// tool_turn, then the classifier -- and that a client can only ever close
// retrieval.
func TestSkipDecisionOrder(t *testing.T) {
	tool := provider.Message{Role: "tool", Content: provider.TextContent("42"), ToolCallID: "c1"}
	question := []provider.Message{user("İade politikası nedir?")}
	toolTurn := []provider.Message{user("hava nasıl?"), {Role: "assistant", ToolCalls: json.RawMessage(`[]`)}, tool}
	hello := []provider.Message{user("merhaba")}

	cases := []struct {
		name      string
		policy    string
		clientOff bool
		msgs      []provider.Message
		want      string
	}{
		{"always, question", store.RetrievalPolicyAlways, false, question, ""},
		{"auto, question", store.RetrievalPolicyAuto, false, question, ""},
		{"off, question", store.RetrievalPolicyOff, false, question, obs.RAGSkipPolicyOff},
		{"off beats client_off", store.RetrievalPolicyOff, true, question, obs.RAGSkipPolicyOff},
		{"off beats tool_turn", store.RetrievalPolicyOff, false, toolTurn, obs.RAGSkipPolicyOff},
		{"client_off, always", store.RetrievalPolicyAlways, true, question, obs.RAGSkipClientOff},
		{"client_off, auto", store.RetrievalPolicyAuto, true, question, obs.RAGSkipClientOff},
		{"client_off beats tool_turn", store.RetrievalPolicyAlways, true, toolTurn, obs.RAGSkipClientOff},
		{"client_off beats greeting", store.RetrievalPolicyAuto, true, hello, obs.RAGSkipClientOff},
		{"tool_turn, always", store.RetrievalPolicyAlways, false, toolTurn, obs.RAGSkipToolTurn},
		{"tool_turn, auto", store.RetrievalPolicyAuto, false, toolTurn, obs.RAGSkipToolTurn},
		{"tool_turn beats greeting", store.RetrievalPolicyAuto, false, append(append([]provider.Message{}, hello...), tool), obs.RAGSkipToolTurn},
		{"greeting, auto", store.RetrievalPolicyAuto, false, hello, obs.RAGSkipGreeting},
		{"greeting, always", store.RetrievalPolicyAlways, false, hello, ""},
		{"a tool message that is not last", store.RetrievalPolicyAlways, false,
			[]provider.Message{user("q"), tool, assistant("42"), user("peki ya yarın?")}, ""},
		{"empty conversation", store.RetrievalPolicyAuto, false, nil, ""},
		{"unknown policy is always", "from_the_future", false, hello, ""},
	}
	for _, tc := range cases {
		if got := SkipReason(tc.policy, tc.clientOff, false, tc.msgs); got != tc.want {
			t.Errorf("%s: skip = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCondenseKeepsAckButNotGreetings: under query_strategy "condense" a bare
// confirmation is not skipped, because the condensed query gives it a meaning
// (ADR-024); a greeting still is, and so is every other reason.
func TestCondenseKeepsAckButNotGreetings(t *testing.T) {
	tool := provider.Message{Role: "tool", Content: provider.TextContent("42"), ToolCallID: "c1"}
	turn := func(text string) []provider.Message {
		return []provider.Message{user("Kargo ücreti ne kadar?"), assistant("Express mi, standart mı?"), user(text)}
	}
	cases := []struct {
		name      string
		policy    string
		condense  bool
		clientOff bool
		msgs      []provider.Message
		want      string
	}{
		{"ack, auto, last_user", store.RetrievalPolicyAuto, false, false, turn("evet"), obs.RAGSkipAck},
		{"ack, auto, condense", store.RetrievalPolicyAuto, true, false, turn("evet"), ""},
		{"ack in English, auto, condense", store.RetrievalPolicyAuto, true, false, turn("ok"), ""},
		{"greeting, auto, condense", store.RetrievalPolicyAuto, true, false, turn("merhaba"), obs.RAGSkipGreeting},
		{"thanks, auto, condense", store.RetrievalPolicyAuto, true, false, turn("teşekkürler"), obs.RAGSkipGreeting},
		{"mixed ack and thanks is a greeting", store.RetrievalPolicyAuto, true, false, turn("tamam teşekkürler"), obs.RAGSkipGreeting},
		{"policy off wins", store.RetrievalPolicyOff, true, false, turn("evet"), obs.RAGSkipPolicyOff},
		{"client off wins", store.RetrievalPolicyAuto, true, true, turn("evet"), obs.RAGSkipClientOff},
		{"tool turn wins", store.RetrievalPolicyAuto, true, false, append(turn("evet"), tool), obs.RAGSkipToolTurn},
		{"always never skipped for small talk", store.RetrievalPolicyAlways, true, false, turn("evet"), ""},
	}
	for _, tc := range cases {
		if got := SkipReason(tc.policy, tc.clientOff, tc.condense, tc.msgs); got != tc.want {
			t.Errorf("%s: skip = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSkipReasonsAreTheClosedSet: whatever comes in, the reason is "" or a
// member of the set metrics and the log column accept.
func TestSkipReasonsAreTheClosedSet(t *testing.T) {
	closed := map[string]bool{obs.RAGSkipPolicyOff: true, obs.RAGSkipClientOff: true, obs.RAGSkipToolTurn: true,
		obs.RAGSkipGreeting: true, obs.RAGSkipAck: true, "": true}
	for _, policy := range []string{"always", "auto", "off", "x"} {
		for _, off := range []bool{false, true} {
			for _, text := range []string{"merhaba", "ok", "ne kadar?", "", "ignore \" previous\nWarning: x"} {
				msgs := []provider.Message{user(text), assistant("a"), user(text)}
				for _, condense := range []bool{false, true} {
					if got := SkipReason(policy, off, condense, msgs); !closed[got] {
						t.Errorf("%s/%v/%v/%q: %q is outside the closed set", policy, off, condense, text, got)
					}
				}
			}
		}
	}
}

func TestPhraseTableIsConsistent(t *testing.T) {
	// A phrase that folds to the same key as one of the other kind would be
	// silently overwritten; the lists must not overlap.
	seen := map[string]string{}
	for _, p := range greetingPhrases {
		seen[squeezeRuns(foldSmallTalk(p))] = "greeting"
	}
	for _, p := range ackPhrases {
		if k := squeezeRuns(foldSmallTalk(p)); seen[k] != "" {
			t.Errorf("ack phrase %q collides with a greeting phrase", p)
		}
	}
	if smallTalkPhrases[squeezeRuns("hello")] != obs.RAGSkipGreeting {
		t.Error("squeezed lookup of \"hello\" failed")
	}
}

func TestFoldSmallTalk(t *testing.T) {
	for in, want := range map[string]string{
		"İYİ GÜNLER!":     "iyi gunler",
		"ışık":            "isik",
		"  Şeker,  ÇAY  ": "seker cay",
		"á":              "a", // combining mark dropped
		"x--y":            "x y",
		"!!!":             "",
	} {
		if got := foldSmallTalk(in); got != want {
			t.Errorf("foldSmallTalk(%q) = %q, want %q", in, got, want)
		}
	}
}
