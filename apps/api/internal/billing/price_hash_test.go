package billing

import (
	"encoding/json"
	"testing"
)

func TestCanonicalJSONSortsNestedKeysAndKeepsValues(t *testing.T) {
	// Map iteration order must not leak into the hash payload.
	input := map[string]any{
		"z": []any{map[string]any{"b": 2, "a": 1}},
		"a": map[string]any{"c": 0.01, "b": true, "a": nil},
	}
	got, err := canonicalJSON(input)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	want := `{"a":{"a":null,"b":true,"c":0.01},"z":[{"a":1,"b":2}]}`
	if string(got) != want {
		t.Fatalf("canonical=%s want %s", got, want)
	}
}

func TestCanonicalJSONDoesNotHTMLEscape(t *testing.T) {
	// Node's JSON.stringify leaves <, > and & literal; the Go encoder must
	// match or a model/tool name containing one would produce a different hash.
	got, err := canonicalJSON(map[string]string{"name": "a&b<c>d"})
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if string(got) != `{"name":"a&b<c>d"}` {
		t.Fatalf("canonical=%s", got)
	}
}

// TestPriceBookHashUsesCanonicalJSON pins the hash of the contract's golden
// Sonnet 5.5 book to the sorted-key canonical bytes. The same JSON through
// roost-website/scripts/generate-prices.mjs --print-hash yields this value, so
// the page hash can equal the API ETag after the page is regenerated from the
// public endpoint.
func TestPriceBookHashUsesCanonicalJSON(t *testing.T) {
	const want = "sha256:159b1370cd8ef4a60ac43d222713949873c972165e869d8faef71cc3950dcc08"
	book := sonnetBook()
	if book.Hash != want {
		t.Fatalf("hash=%q want %q", book.Hash, want)
	}
	// The public view is valid JSON and carries no cost field, so the same
	// bytes can be hashed by the website generator.
	publicJSON, err := json.Marshal(book.PublicView())
	if err != nil {
		t.Fatalf("marshal public: %v", err)
	}
	if string(publicJSON) == "" || json.Valid(publicJSON) == false {
		t.Fatalf("public view is not valid JSON: %s", publicJSON)
	}
}
