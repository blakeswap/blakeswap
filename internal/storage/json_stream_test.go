package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type streamEmbedded struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
}
type streamSample struct {
	streamEmbedded
	Nil    *streamEmbedded            `json:"nil"`
	Value  *streamEmbedded            `json:"value"`
	Number *int64                     `json:"number"`
	Maps   map[string]*streamEmbedded `json:"maps"`
	Empty  []string                   `json:"empty"`
	Absent []string                   `json:"absent,omitempty"`
	Raw    json.RawMessage            `json:"raw"`
	Bytes  []byte                     `json:"bytes"`
}

func TestJSONStreamPreservesTypedRecordsAndNulls(t *testing.T) {
	number := int64(9223372036854775807)
	sample := streamSample{streamEmbedded: streamEmbedded{ID: "<identity>", Count: number}, Value: &streamEmbedded{ID: "nested", Count: number}, Number: &number, Maps: map[string]*streamEmbedded{"null": nil, "record": {ID: "record"}}, Empty: []string{}, Raw: json.RawMessage(`{"unknown":[null,9223372036854775807]}`), Bytes: []byte{1, 2, 3}}
	var b bytes.Buffer
	if err := writeJSONStream(context.Background(), &b, sample); err != nil {
		t.Fatal(err)
	}
	var canonical, streamed any
	raw, _ := json.Marshal(sample)
	if err := json.Unmarshal(raw, &canonical); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b.Bytes(), &streamed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(canonical, streamed) {
		t.Fatalf("wire shapes differ: %s / %s", raw, b.Bytes())
	}
	var recovered streamSample
	if err := readJSONStream(context.Background(), &b, &recovered); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sample, recovered) {
		t.Fatalf("typed roundtrip differs: %+v / %+v", sample, recovered)
	}
}
func TestJSONStreamRejectsAmbiguousOrIncompleteDataBeforePublication(t *testing.T) {
	for _, raw := range []string{`{"id":"one","id":"two"}`, `{"id":"one"} {}`, `{"id":"one"`, `{"unknown":1}`, `{"count":9223372036854775808}`} {
		out := streamEmbedded{ID: "unchanged"}
		if err := readJSONStream(context.Background(), strings.NewReader(raw), &out); err == nil {
			t.Fatal("accepted", raw)
		}
		if out.ID != "unchanged" {
			t.Fatal("partially published", raw)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeJSONStream(ctx, &bytes.Buffer{}, streamEmbedded{}); err == nil {
		t.Fatal("cancellation ignored")
	}
	if err := writeJSONStream(context.Background(), &bytes.Buffer{}, json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid raw record accepted")
	}
}
