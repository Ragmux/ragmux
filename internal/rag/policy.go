package rag

import (
	"strings"
	"unicode"

	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// SkipReason decides whether a chat request of a project that has a RAG store
// skips retrieval, and says why (ADR-024). The empty string means "retrieve".
//
// The order is fixed and the first match wins:
//
//  1. the project's retrieval_policy is "off"          -> policy_off
//  2. the client sent ragmux.retrieval = "off"         -> client_off
//  3. the last message has the "tool" role             -> tool_turn
//  4. the policy is "auto" and the turn is small talk  -> greeting | ack
//
// Steps 2 and 3 apply under every policy; a client can only ever turn
// retrieval off, never on, so clientOff cannot reopen a "off" project and no
// client value exists that overrides "auto" or "off". Any other policy value
// behaves as "always".
//
// The result is one of the obs.RAGSkip* constants: it is stored in
// request_logs.rag_skip_reason and used as a metric label, so it must stay in
// that closed set.
func SkipReason(policy string, clientOff bool, msgs []provider.Message) string {
	switch {
	case policy == store.RetrievalPolicyOff:
		return obs.RAGSkipPolicyOff
	case clientOff:
		return obs.RAGSkipClientOff
	case len(msgs) > 0 && msgs[len(msgs)-1].Role == "tool":
		// A tool turn feeds a tool result back to the model: the question was
		// already answered with context on the turn that made the tool call.
		return obs.RAGSkipToolTurn
	case policy == store.RetrievalPolicyAuto:
		return classifyTurn(msgs)
	}
	return ""
}

// classifyTurn is the rule-based "auto" classifier. It looks at the last user
// message only: a short greeting, thanks or acknowledgement has nothing in the
// store to find. A question the user asks again is retrieved again (ADR-024):
// the context of the first answer is injected into the upstream request only
// and is not in the client's history, so skipping would answer without it.
func classifyTurn(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		text := foldSmallTalk(msgs[i].Text())
		if text == "" || len(text) > maxSmallTalkBytes {
			return ""
		}
		return smallTalkKind(text)
	}
	return ""
}

// Limits of the small-talk matcher: a message longer than either is a real
// question however it starts.
const (
	maxSmallTalkBytes = 64
	maxSmallTalkWords = 6
)

// smallTalkKind returns obs.RAGSkipGreeting or obs.RAGSkipAck when text (already
// normalised) is made up entirely of known phrases, and "" otherwise. "Merhaba,
// iade nasıl yapılır?" is not: one unknown word makes it a question.
//
// Greetings, farewells and thanks are the greeting kind; bare confirmations
// are the ack kind. A text that mixes both ("tamam teşekkürler") is a greeting.
func smallTalkKind(text string) string {
	words := strings.Fields(squeezeRuns(text))
	if len(words) == 0 || len(words) > maxSmallTalkWords {
		return ""
	}
	// reach[i] says the first i words are covered by phrases; greet[i] that at
	// least one of those phrases is a greeting.
	reach := make([]bool, len(words)+1)
	greet := make([]bool, len(words)+1)
	reach[0] = true
	for i := 0; i < len(words); i++ {
		if !reach[i] {
			continue
		}
		for l := 1; l <= maxPhraseWords && i+l <= len(words); l++ {
			kind, ok := smallTalkPhrases[strings.Join(words[i:i+l], " ")]
			if !ok {
				continue
			}
			reach[i+l] = true
			greet[i+l] = greet[i+l] || greet[i] || kind == obs.RAGSkipGreeting
		}
	}
	switch {
	case !reach[len(words)]:
		return ""
	case greet[len(words)]:
		return obs.RAGSkipGreeting
	}
	return obs.RAGSkipAck
}

// foldSmallTalk folds a message to the form the matcher
// compares: lower case, Turkish-insensitive, letters and digits only, single
// spaces. "İYİ GÜNLER!", "iyi günler" and "IYI GUNLER" all become "iyi gunler".
//
// The fold is deliberately lossy in both directions of the Turkish dotted and
// dotless i (İ, I, ı and i are one letter) and maps ş ç ğ ö ü to their ASCII
// base, because the same users write the same words with and without Turkish
// keyboards; the phrase list below is folded the same way, so only phrases
// that coincide after folding can collide. Anything that is not a letter or a
// digit -- punctuation, symbols, emoji, combining marks excepted -- is a word
// separator.
func foldSmallTalk(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := true // leading separators are dropped
	for _, r := range s {
		switch {
		case unicode.IsMark(r):
			// A decomposed "İ" is "I" plus U+0307: the mark folds away.
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(foldTurkishRune(r))
			space = false
		case !space:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSuffix(b.String(), " ")
}

// foldTurkishRune lower-cases r and folds the Turkish letters to ASCII.
func foldTurkishRune(r rune) rune {
	switch r {
	case 'İ', 'I', 'ı':
		return 'i'
	case 'ş', 'Ş':
		return 's'
	case 'ç', 'Ç':
		return 'c'
	case 'ğ', 'Ğ':
		return 'g'
	case 'ö', 'Ö':
		return 'o'
	case 'ü', 'Ü', 'û', 'Û':
		return 'u'
	case 'â', 'Â':
		return 'a'
	case 'î', 'Î':
		return 'i'
	}
	return unicode.ToLower(r)
}

// squeezeRuns collapses every run of one letter to a single letter, so a
// stretched "meeerhaba" or "teşekkürlerrr" meets the phrase "merhaba" /
// "teşekkürler". Phrases are squeezed the same way, which is what keeps
// "hello" matching: it is looked up as "helo". Digits are left alone.
func squeezeRuns(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var prev rune
	for _, r := range s {
		if r == prev && unicode.IsLetter(r) {
			continue
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

// maxPhraseWords is the length in words of the longest phrase below; the
// matcher never looks further.
const maxPhraseWords = 4

// smallTalkPhrases maps a folded, squeezed phrase to its kind. It is built
// once from the readable lists below.
var smallTalkPhrases = buildPhrases()

func buildPhrases() map[string]string {
	out := map[string]string{}
	add := func(kind string, phrases []string) {
		for _, p := range phrases {
			key := squeezeRuns(foldSmallTalk(p))
			if n := len(strings.Fields(key)); n > maxPhraseWords {
				panic("rag: small-talk phrase longer than maxPhraseWords: " + p)
			}
			out[key] = kind
		}
	}
	add(obs.RAGSkipGreeting, greetingPhrases)
	add(obs.RAGSkipAck, ackPhrases)
	return out
}

// greetingPhrases are greetings, farewells and thanks, Turkish and English. A
// phrase is a whole message or a part of one: "merhaba nasılsın" is two of them.
var greetingPhrases = []string{
	// Turkish greetings and farewells.
	"merhaba", "merhabalar", "mrb", "selam", "selamlar", "slm", "selamun aleyküm", "günaydın", "tünaydın",
	"iyi günler", "iyi akşamlar", "iyi geceler", "iyi sabahlar", "hayırlı günler", "hoş geldin", "hoşgeldin",
	"hoş geldiniz", "hoşgeldiniz", "hoş bulduk", "nasılsın", "nasılsınız", "naber", "ne haber", "nasıl gidiyor",
	"görüşürüz", "görüşmek üzere", "hoşça kal", "hoşçakal", "güle güle", "kendine iyi bak",
	// Turkish thanks.
	"teşekkürler", "teşekkür ederim", "teşekkür ederiz", "çok teşekkürler", "çok teşekkür ederim", "sağ ol",
	"sağol", "sağ olun", "sağolun", "çok sağ ol", "çok sağol", "eyvallah", "tşk", "mersi", "eline sağlık",
	// English greetings and farewells.
	"hello", "hi", "hey", "hiya", "yo", "howdy", "hello there", "hi there", "hey there", "greetings",
	"good morning", "good afternoon", "good evening", "good night", "good day", "how are you",
	"how are you doing", "how is it going", "hows it going", "what's up", "whats up", "sup", "bye",
	"goodbye", "good bye", "see you", "see ya", "take care",
	// English thanks.
	"thanks", "thank you", "thx", "ty", "thanks a lot", "thank you so much", "thanks so much",
	"thank you very much", "many thanks", "cheers", "much appreciated", "appreciated",
}

// ackPhrases are bare confirmations a user answers an assistant turn with.
var ackPhrases = []string{
	"ok", "okay", "okey", "oke", "tamam", "tamamdır", "tmm", "evet", "hayır", "olur", "peki", "anladım",
	"anlaşıldı", "anlaştık", "güzel", "harika", "süper", "mükemmel",
	"yes", "no", "yeah", "yep", "yup", "nope", "sure", "got it", "alright", "all right", "fine", "cool",
	"great", "nice", "perfect", "understood", "i see", "makes sense", "noted",
}
