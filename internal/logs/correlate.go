package logs

import (
	"net/url"
	"regexp"
	"strings"
)

// Correlator finds the correlation id in an entry: in JSON fields or query
// parameters named by Keys, else by Pattern in the text.
type Correlator struct {
	Keys    []string
	Pattern *regexp.Regexp
}

// FromFields returns the first Keys field holding a string or number.
func (c *Correlator) FromFields(fields map[string]any) string {
	if c == nil {
		return ""
	}
	for _, k := range c.Keys {
		switch v := fields[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case interface{ String() string }: // json.Number
			return v.String()
		}
	}
	return ""
}

// FromTarget looks in a request target's query string, then in the target
// itself with Pattern (ids in paths, like /runs/<id>).
func (c *Correlator) FromTarget(target string) string {
	if c == nil {
		return ""
	}
	if _, query, ok := strings.Cut(target, "?"); ok && len(c.Keys) > 0 {
		if values, err := url.ParseQuery(query); err == nil {
			for _, k := range c.Keys {
				if v := values.Get(k); v != "" {
					return v
				}
			}
		}
	}
	return c.FromText(target)
}

func (c *Correlator) FromText(s string) string {
	if c == nil || c.Pattern == nil {
		return ""
	}
	m := c.Pattern.FindStringSubmatch(s)
	switch {
	case m == nil:
		return ""
	case len(m) > 1:
		return m[1]
	}
	return m[0]
}
