package proxy

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

const mask = "[redacted]"

func sensitive(key string, redact []string) bool {
	k := strings.ToLower(key)
	for _, r := range redact {
		if strings.Contains(k, strings.ToLower(r)) {
			return true
		}
	}
	return false
}

// redactJSON masks values under sensitive keys at any depth. Bodies that
// don't decode (truncated ones) are returned as they are.
func redactJSON(data []byte, redact []string) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if len(redact) == 0 || dec.Decode(&v) != nil {
		return string(data)
	}
	v = redactValue(v, redact)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return string(data)
	}
	return strings.TrimRight(buf.String(), "\n")
}

func redactValue(v any, redact []string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitive(k, redact) {
				t[k] = mask
			} else {
				t[k] = redactValue(val, redact)
			}
		}
	case []any:
		for i := range t {
			t[i] = redactValue(t[i], redact)
		}
	}
	return v
}

// redactForm masks sensitive keys in a urlencoded string, keeping the
// original order and encoding of everything else.
func redactForm(encoded string, redact []string) string {
	pairs := strings.Split(encoded, "&")
	for i, pair := range pairs {
		k, _, hasValue := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(k)
		if err != nil {
			name = k
		}
		if hasValue && sensitive(name, redact) {
			pairs[i] = k + "=" + url.QueryEscape(mask)
		}
	}
	return strings.Join(pairs, "&")
}

func redactQuery(requestURI string, redact []string) string {
	p, query, ok := strings.Cut(requestURI, "?")
	if !ok {
		return requestURI
	}
	return p + "?" + redactForm(query, redact)
}
