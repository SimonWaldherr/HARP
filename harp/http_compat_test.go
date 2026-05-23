package harp

import (
	"bytes"
	"net/http"
	"testing"
)

func TestHTTPHeaderFromProtoPreservesRepeatedHeaders(t *testing.T) {
	values := []*HTTPHeader{{
		Name:   "Set-Cookie",
		Values: []string{"a=1", "b=2"},
	}}

	headers := HTTPHeaderFromProto(map[string]string{"Set-Cookie": "legacy=1"}, values)
	got := headers.Values("Set-Cookie")
	if len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
		t.Fatalf("expected repeated Set-Cookie values to be preserved, got %#v", got)
	}
}

func TestHeaderValuesFromHTTPRoundTrip(t *testing.T) {
	headers := http.Header{}
	headers.Add("Accept", "application/json")
	headers.Add("Accept", "text/event-stream")

	roundTrip := HTTPHeaderFromProto(nil, HeaderValuesFromHTTP(headers))
	got := roundTrip.Values("Accept")
	if len(got) != 2 || got[0] != "application/json" || got[1] != "text/event-stream" {
		t.Fatalf("expected repeated Accept values to round trip, got %#v", got)
	}
}

func TestBodyBytesFromProtoPrefersBinaryBody(t *testing.T) {
	got := BodyBytesFromProto("legacy", []byte{0, 1, 2})
	if !bytes.Equal(got, []byte{0, 1, 2}) {
		t.Fatalf("expected binary body to win, got %#v", got)
	}
}
