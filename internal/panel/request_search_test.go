package panel

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRequestSearchIncludesUpstreamDiagnostics(t *testing.T) {
	source, err := filepath.Abs("../../frontend/src/requestSearch.ts")
	if err != nil {
		t.Fatal(err)
	}
	bin, args, ok := nodeTSRunner(t, source)
	if !ok {
		t.Skip("node TypeScript loading unavailable")
	}
	const script = `
import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
const { requestMatchesQuery } = await import(pathToFileURL(process.argv[1]).href);
assert.equal(requestMatchesQuery({upstream_code:'6021'}, '6021'), true);
assert.equal(requestMatchesQuery({upstream_kind:'model_queued'}, 'MODEL_QUEUED'), true);
assert.equal(requestMatchesQuery({upstream_code:'6021', model:'glm-5.3'}, '6021 glm'), true);
assert.equal(requestMatchesQuery({upstream_code:'6021'}, '6020'), false);
assert.equal(requestMatchesQuery({}, ''), true);
`
	argv := append(append([]string{}, args...), "--input-type=module", "-e", script, source)
	if out, err := exec.Command(bin, argv...).CombinedOutput(); err != nil {
		t.Fatalf("search: %v\n%s", err, out)
	}
}
