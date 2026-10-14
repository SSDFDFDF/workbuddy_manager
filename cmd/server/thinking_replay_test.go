package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"workbuddy_manager/internal/config"
	"workbuddy_manager/internal/media"
)

func TestThinkingReplayConfigSaveAndHotApply(t *testing.T) {
	if config.Default().Upstream.ReplayThinking || upstreamOptions(config.Default()).ReplayThinking {
		t.Fatal("thinking replay must default off at startup")
	}
	tool, image := media.CurrentToolPolicy(), media.CurrentImagePolicy()
	t.Cleanup(func() {
		_ = media.SetToolPolicy(tool)
		_ = media.SetImagePolicy(image)
	})
	hot := testHotTargets()
	defer hot.requestLog.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		payload := []byte(`{"upstream":{"replay_thinking":` + strconv.FormatBool(enabled) + `}}`)
		if _, err := saveConfig(payload, path, hot); err != nil {
			t.Fatal(err)
		}
		if hot.upstream.ReplayThinkingNow() != enabled {
			t.Fatal("config save failed to apply live policy")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.ParseConfig(b)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Upstream.ReplayThinking != enabled || upstreamOptions(cfg).ReplayThinking != enabled {
			t.Fatal("restart policy differs from saved live policy")
		}
		if _, err := saveConfig([]byte(`{"upstream":{"replay_thinking":"true"}}`), path, hot); err == nil {
			t.Fatal("string is not a boolean switch")
		}
		if hot.upstream.ReplayThinkingNow() != enabled {
			t.Fatal("invalid save changed policy")
		}
	}
}
