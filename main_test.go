package main

import (
	"testing"

	"github.com/shivamsingh262/tailyard/internal/config"
)

// The example written by -init must always be a valid config.
func TestExampleConfigParses(t *testing.T) {
	cfg, err := config.Parse(exampleConfig, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) == 0 || cfg.Proxy == nil || cfg.Redis == nil || len(cfg.Actions) == 0 {
		t.Errorf("example config lost a section: %+v", cfg)
	}
}
