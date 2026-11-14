package bundle

import (
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/internal/container"
)

// Container classes come from wrangler "containers" (Cloudflare's shape)
// with twd.jsonc "containers" overriding by class; each must be a DO class.
func TestParseContainers(t *testing.T) {
	wrangler := `{
	  "name": "sb", "main": "index.js",
	  // Cloudflare's examples/minimal
	  "containers": [{ "class_name": "Sandbox", "image": "./Dockerfile", "instance_type": "lite", "max_instances": 1 }],
	  "durable_objects": { "bindings": [{ "name": "Sandbox", "class_name": "Sandbox" }] },
	}`
	files := map[string][]byte{"wrangler.jsonc": []byte(wrangler), "index.js": []byte("export default {}"),
		"twd.jsonc": []byte(`{ "containers": [{ "class": "Sandbox", "instance_type": "basic" }] }`)}
	a, err := Parse(files)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := a.Containers["Sandbox"]
	if !ok || c.Image != "./Dockerfile" || c.MaxInstances != 1 || c.Resources() != container.InstanceTypes["basic"] {
		t.Fatalf("%+v", a.Containers)
	}
	files["twd.jsonc"] = []byte(`{ "containers": [{ "class": "Other", "image": "x" }] }`)
	if _, err := Parse(files); err == nil || !strings.Contains(err.Error(), "no durable_objects binding") {
		t.Fatalf("container without a DO class: %v", err)
	}
	node := map[string][]byte{"twd.jsonc": []byte(`{"runtime":"node","main":"s.mjs","containers":[{"class":"S","image":"x"}]}`), "s.mjs": nil}
	if _, err := Parse(node); err == nil || !strings.Contains(err.Error(), "no containers") {
		t.Fatalf("node runtime with containers: %v", err)
	}
}
