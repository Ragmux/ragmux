package gateway

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNoteResponseModel(t *testing.T) {
	cases := []struct {
		name                   string
		upstream, client, conn string
		want                   string
	}{
		{"upstream snapshot wins", "gpt-4o-2024-08-06", "gpt-4o", "gpt-4o", "gpt-4o-2024-08-06"},
		{"empty falls back to connection", "", "anything", "gpt-4o", "gpt-4o"},
		{"echo of client string is not trusted", "client-chose-this", "client-chose-this", "claude-sonnet", "claude-sonnet"},
		{"echo equal to connection model is fine", "gpt-4o", "gpt-4o", "gpt-4o", "gpt-4o"},
	}
	for _, c := range cases {
		var o requestObs
		o.noteResponseModel(c.upstream, c.client, c.conn)
		if o.responseModel != c.want {
			t.Errorf("%s: responseModel = %q, want %q", c.name, o.responseModel, c.want)
		}
	}
}

func TestNoteResponseModelKeepsFirstAndCaps(t *testing.T) {
	var o requestObs
	o.noteResponseModel("first", "c", "conn")
	o.noteResponseModel("second", "c", "conn")
	if o.responseModel != "first" {
		t.Fatalf("responseModel = %q, want the first value kept", o.responseModel)
	}

	var long requestObs
	long.noteResponseModel(strings.Repeat("a", maxResponseModelLen-1)+"ğğğ", "c", "conn")
	if len(long.responseModel) > maxResponseModelLen || !utf8.ValidString(long.responseModel) {
		t.Fatalf("response model not capped on a rune boundary: len %d, valid %v",
			len(long.responseModel), utf8.ValidString(long.responseModel))
	}
}

func TestNoteFinishReasonIsBoundedAndDistinct(t *testing.T) {
	var o requestObs
	for _, r := range []string{"stop", "stop", "length", "made_up_by_upstream", "another_one", ""} {
		r := r
		o.noteFinishReason(&r)
	}
	o.noteFinishReason(nil)
	want := []string{"stop", "length", "other"}
	if !reflect.DeepEqual(o.finishReasons, want) {
		t.Fatalf("finishReasons = %v, want %v", o.finishReasons, want)
	}
}
