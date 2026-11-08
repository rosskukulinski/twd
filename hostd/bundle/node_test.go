package bundle

import (
	"strings"
	"testing"
)

func TestParseNode(t *testing.T) {
	files := map[string][]byte{
		"twd.jsonc":               []byte(`{"name":"n","runtime":"node","main":"src/s.ts","node":{"env":{"A":"1"}},"tailnet":{"tags":["tag:x"]}}`),
		"twd-bundle.json":         []byte(`{"main":"dist/server.mjs","env":{"B":"2"}}`),
		"dist/server.mjs":         []byte(`console.log(1)`),
		"node_modules/x/index.js": []byte(`1`),
	}
	a, err := Parse(files)
	if err != nil {
		t.Fatal(err)
	}
	if a.Runtime != RuntimeNode || a.Node.Main != "dist/server.mjs" || a.Node.Listen != "tcp" || a.Node.Health != "/" ||
		a.Node.Env["A"] != "1" || a.Node.Env["B"] != "2" || a.Node.Files["node_modules/x/index.js"] == nil || a.WorkloadTags[0] != "tag:x" {
		t.Fatalf("%+v %+v", a, a.Node)
	}
	bad := map[string]string{
		`{"runtime":"node","main":"dist/server.mjs","tailnet":{"objects":{"A":{}}}}`: "tailnet.objects",
		`{"runtime":"node","main":"dist/server.mjs","tier":"microvm"}`:               "process tier only",
		`{"runtime":"node"}`:                   `needs "main"`,
		`{"runtime":"node","main":"nope.mjs"}`: "not in bundle",
		`{"runtime":"node","main":"dist/server.mjs","node":{"env":{"TWD_X":"1"}}}`: "reserved",
		`{"runtime":"node","main":"dist/server.mjs","node":{"args":["-e","x"]}}`:   "--flags",
		`{"runtime":"node","main":"dist/server.mjs","node":{"listen":"udp"}}`:      "node.listen",
		`{"runtime":"deno","main":"dist/server.mjs"}`:                              "runtime",
	}
	for cfg, want := range bad {
		_, err := Parse(map[string][]byte{"twd.jsonc": []byte(cfg), "dist/server.mjs": []byte("1")})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", cfg, want, err)
		}
	}
}
