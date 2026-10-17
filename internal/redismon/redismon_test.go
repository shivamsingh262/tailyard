package redismon

import (
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
)

func TestStreamIDLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"", "1-0", true},
		{"1-0", "", false},
		{"1-0", "1-1", true},
		{"2-0", "10-0", true}, // numeric, not lexical
		{"10-0", "2-0", false},
	}
	for _, c := range cases {
		if got := streamIDLess(c.a, c.b); got != c.want {
			t.Errorf("streamIDLess(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestKeyPrefix(t *testing.T) {
	for key, want := range map[string]string{"user:1": "user", "cache.x.y": "cache", "plain": "(no prefix)", ":odd": "(no prefix)"} {
		if got := keyPrefix(key); got != want {
			t.Errorf("keyPrefix(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestGlob(t *testing.T) {
	re := globRegexp("events:*:log")
	if !re.MatchString("events:abc:log") || re.MatchString("events:abc") || re.MatchString("xevents:a:log") {
		t.Error("glob mismatch")
	}
}

func TestTailTargetAndIDOf(t *testing.T) {
	m := New(&config.Redis{Addr: "127.0.0.1:1", Tail: "run:*:events"})
	defer m.Close()
	if got := m.pinnedStream("r42"); got != "run:r42:events" {
		t.Errorf("pinnedStream = %q", got)
	}
	if got := m.pinnedStream(""); got != "" {
		t.Errorf("pinnedStream with nothing pinned = %q", got)
	}
	if got := m.idOf("run:r42:events"); got != "r42" {
		t.Errorf("idOf = %q", got)
	}
	if got := m.idOf("other"); got != "" {
		t.Errorf("idOf(other) = %q", got)
	}
}

func TestEventLine(t *testing.T) {
	m := New(&config.Redis{Addr: "127.0.0.1:1", Tail: "events:*"})
	defer m.Close()
	l := m.eventLine("events:abc", redis.XMessage{ID: "1700000000000-0", Values: map[string]any{
		"event_type": "step_failed",
		"data":       `{"step":"resize","attempt":2}`,
	}})
	if l.Level != logs.LError || l.Corr != "abc" {
		t.Errorf("line: %+v", l)
	}
	if l.Plain != "step_failed attempt=2 step=resize" {
		t.Errorf("plain = %q", l.Plain)
	}
	if !strings.Contains(strings.Join(l.Detail, "\n"), `"resize"`) {
		t.Errorf("detail = %q", l.Detail)
	}
}

func TestResp(t *testing.T) {
	if got := string(resp("AUTH", "p w")); got != "*2\r\n$4\r\nAUTH\r\n$3\r\np w\r\n" {
		t.Errorf("got %q", got)
	}
}
