package bundle

import (
	"strings"
	"testing"
)

func cronFiles(wrangler string) map[string][]byte {
	return map[string][]byte{
		"wrangler.jsonc": []byte(wrangler),
		"twd.jsonc":      []byte(`{"tailnet":{"tags":["tag:x"]}}`),
		"src/index.js":   []byte(`export default { scheduled() {} }`),
	}
}

func TestParseCrons(t *testing.T) {
	a, err := Parse(cronFiles(`{
		"name": "c", "main": "src/index.js",
		// JSONC comments and trailing commas, as wrangler allows
		"triggers": { "crons": ["*/5 * * * *", "0 18 * * friL",], },
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Crons, "|") != "*/5 * * * *|0 18 * * friL" {
		t.Fatalf("crons = %q", a.Crons)
	}
	a, err = Parse(cronFiles(`{"name":"c","main":"src/index.js"}`))
	if err != nil || len(a.Crons) != 0 {
		t.Fatalf("no triggers: %v %q", err, a.Crons)
	}
	a, err = Parse(cronFiles(`{"name":"c","main":"src/index.js","triggers":{"crons":[]}}`))
	if err != nil || len(a.Crons) != 0 {
		t.Fatalf("empty triggers (wrangler's way to remove them): %v %q", err, a.Crons)
	}
}

func TestParseCronsInvalid(t *testing.T) {
	many := `"* * * * *"` + strings.Repeat(`,"* * * * *"`, 10)
	for cfg, want := range map[string]string{
		`{"triggers":{"crons":["* * * *"]}}`:               "want 5 fields",
		`{"triggers":{"crons":["* * * * 0"]}}`:             "1 = Sunday",
		`{"triggers":{"crons":["0 0 30 2 *"]}}`:            "never fires",
		`{"triggers":{"crons":["* * * * *","* * * * *"]}}`: "twice",
		`{"triggers":{"crons":[` + many + `]}}`:            "at most",
		`{"triggers":{"crons":"* * * * *"}}`:               "cannot unmarshal",
		`{"triggers":{"crons":[5]}}`:                       "cannot unmarshal",
	} {
		_, err := Parse(cronFiles(`{"name":"c","main":"src/index.js",` + strings.TrimPrefix(cfg, "{")))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", cfg, want, err)
		}
	}
}

func TestParseCronsNodeRuntime(t *testing.T) {
	files := map[string][]byte{
		"twd.jsonc":       []byte(`{"runtime":"node","main":"dist/server.mjs","tailnet":{"tags":["tag:x"]}}`),
		"wrangler.jsonc":  []byte(`{"triggers":{"crons":["* * * * *"]}}`),
		"dist/server.mjs": []byte(`1`),
	}
	if _, err := Parse(files); err == nil || !strings.Contains(err.Error(), "runtime \"node\" does not support") {
		t.Fatalf("node + crons: %v", err)
	}
	files["wrangler.jsonc"] = []byte(`{"name":"n"}`)
	if _, err := Parse(files); err != nil {
		t.Fatalf("node without crons: %v", err)
	}
}
