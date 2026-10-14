package panel

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestThinkingReplayFormBoolean(t *testing.T) {
	schema, err := filepath.Abs(schemaRel)
	if err != nil {
		t.Fatal(err)
	}
	bin, args, ok := nodeTSRunner(t, schema)
	if !ok {
		t.Skip("node TypeScript loading unavailable")
	}
	const script = `
import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
const { CFG_MAP, collectConfig } = await import(pathToFileURL(process.argv[1]).href);
assert.deepEqual(CFG_MAP.replay_thinking, ['upstream', 'replay_thinking']);
for (const checked of [true, false]) {
  const cfg = collectConfig({replay_thinking: {type: 'checkbox', checked}});
  assert.equal(cfg.upstream.replay_thinking, checked);
}
assert.equal(collectConfig({}).upstream, undefined);
`
	argv := append(append([]string{}, args...), "--input-type=module", "-e", script, schema)
	if out, err := exec.Command(bin, argv...).CombinedOutput(); err != nil {
		t.Fatalf("thinking replay form: %v\n%s", err, out)
	}
}
