package panel

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEffortOverrideFormValidation(t *testing.T) {
	schema, err := filepath.Abs(schemaRel)
	if err != nil {
		t.Fatal(err)
	}
	bin, args, ok := nodeTSRunner(t, schema)
	if !ok {
		t.Skip("node 不可用或不支持 TS 直载")
	}
	const script = `
import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
const { parseEffortOverrides, formatEffortOverrides, collectConfig } = await import(pathToFileURL(process.argv[1]).href);
for (const text of [
  'glm-5.2 = high\nGLM-5.2 = xhigh',
  'cn:glm-5.2 = high\n CN:GLM-5.2 = low',
  '* = low\n * = high',
]) {
  const parsed = parseEffortOverrides(text);
  assert.equal(parsed.bad.length, 1);
  assert.match(parsed.bad[0], /归一后重复/);
}
const parsed = parseEffortOverrides('# comment\n GLM-5.2 = XHigh\nglobal:glm-5.2 = high');
assert.deepEqual(parsed.bad, []);
assert.equal(parsed.entries['glm-5.2'], 'xhigh');
assert.equal(parsed.entries['global:glm-5.2'], 'high');
assert.equal(formatEffortOverrides(parsed.entries), 'glm-5.2 = xhigh\nglobal:glm-5.2 = high');
const special = parseEffortOverrides('__proto__ = high\nconstructor = low');
assert.deepEqual(special.bad, []);
assert.equal(JSON.parse(JSON.stringify(special.entries)).__proto__, 'high');
assert.equal(special.entries.constructor, 'low');
assert.equal(parseEffortOverrides('bad line').bad.length, 1);
const cleared = collectConfig({effort_overrides_text: {value: ''}});
assert.equal(JSON.stringify(cleared.upstream.effort_default_overrides), '{}');
assert.equal(collectConfig({}).upstream, undefined);
`
	argv := append(append([]string{}, args...), "--input-type=module", "-e", script, schema)
	if out, err := exec.Command(bin, argv...).CombinedOutput(); err != nil {
		t.Fatalf("effort override form validation: %v\n%s", err, out)
	}
}
