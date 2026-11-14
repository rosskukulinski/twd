package container

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	wr := []byte(`{"containers": [
		{"class_name": "Sandbox", "image": "./Dockerfile", "instance_type": "basic", "max_instances": 3},
		{"class_name": "Big", "image": "docker.io/cloudflare/sandbox:0.12.14", "instance_type": {"vcpu": 2, "memory_mib": 3072, "disk_mb": 9000}}
	]}`)
	twd := []byte(`{"containers": [{"class": "Sandbox", "arch": "amd64", "max_instances": 1}, {"class": "Lite", "image": "img"}]}`)
	cs, err := Parse(wr, twd)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[0].Class != "Big" || cs[1].Class != "Lite" || cs[2].Class != "Sandbox" {
		t.Fatalf("%+v", cs)
	}
	if r := cs[0].Resources(); r != (Resources{VCPU: 2, MemMiB: 3072, DiskMiB: 9000}) {
		t.Fatalf("custom: %+v", r)
	}
	if r := cs[1].Resources(); r != InstanceTypes["lite"] {
		t.Fatalf("default type: %+v", r)
	}
	s := cs[2]
	if s.Image != "./Dockerfile" || s.MaxInstances != 1 || s.Arch != "amd64" || s.Resources() != InstanceTypes["basic"] {
		t.Fatalf("twd.jsonc override: %+v", s)
	}
	if a, err := Arch(cs); err != nil || a != "amd64" {
		t.Fatalf("arch %q %v", a, err)
	}
	if _, err := Arch([]Class{{Arch: "amd64"}, {Arch: "arm64"}}); err == nil {
		t.Fatal("mixed arches accepted")
	}
	for in, want := range map[string]string{
		`{"containers":[{"class_name":"X"}]}`:                                        "image is required",
		`{"containers":[{"class_name":"1x","image":"i"}]}`:                           "not a class name",
		`{"containers":[{"class_name":"X","image":"i","instance_type":"huge"}]}`:     "instance_type",
		`{"containers":[{"class_name":"X","image":"i","instance_type":{"vcpu":0}}]}`: "positive",
		`{"containers":[{"class_name":"X","image":"i","max_instances":-1}]}`:         "max_instances",
		`{"containers":[{"class_name":"X","image":"i","arch":"riscv64"}]}`:           "arch",
	} {
		if _, err := Parse([]byte(in), nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
	if cs, err := Parse(nil, []byte(`{"tier": "process"}`)); err != nil || len(cs) != 0 {
		t.Fatalf("no containers: %v %v", cs, err)
	}
}

func TestResourcesFits(t *testing.T) {
	held := InstanceTypes["basic"]
	if !InstanceTypes["lite"].Fits(held) || !held.Fits(held) || InstanceTypes["standard-1"].Fits(held) {
		t.Fatal("Fits")
	}
}
