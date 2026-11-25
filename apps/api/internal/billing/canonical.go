package billing

import (
	"bytes"
	"encoding/json"
	"sort"
)

// canonicalJSON renders v as deterministic JSON: object keys sorted at every
// level, arrays keep their order, no insignificant whitespace, UTF-8.
//
// This is the canonicalization the marketing site uses in
// roost-website/scripts/generate-prices.mjs (bookHash): both sides must hash
// the same bytes for the page hash and the API hash to agree. encoding/json
// emits struct fields in declaration order, so marshalling the view directly
// (the previous implementation) produced a different hash for identical
// content. Round-tripping through `any` lets the writer sort every object.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Preserve integer values exactly instead of turning them into float64.
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, decoded); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSONString(buf, key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonicalJSON(buf, value[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case string:
		return writeJSONString(buf, value)
	case json.Number:
		buf.WriteString(value.String())
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		buf.Write(raw)
	}
	return nil
}

// writeJSONString mirrors JSON.stringify: no HTML escaping (<, > and & stay
// literal), which keeps the Go and Node canonical bytes identical.
func writeJSONString(buf *bytes.Buffer, value string) error {
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return err
	}
	raw := encoded.Bytes()
	// Encoder appends a newline; canonical JSON must not.
	buf.Write(raw[:len(raw)-1])
	return nil
}
