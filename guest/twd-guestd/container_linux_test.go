//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func colon(s string) [][]string {
	var out [][]string
	for _, ln := range strings.Split(strings.TrimSpace(s), "\n") {
		out = append(out, strings.Split(ln, ":"))
	}
	return out
}

// The image's USER (and exec's user) must resolve the way Docker does, and
// an unknown name must fail rather than silently run as root.
func TestLookupUser(t *testing.T) {
	passwd := colon("root:x:0:0:root:/root:/bin/sh\nnode:x:1000:1000::/home/node:/bin/sh\nsvc:x:999:998::/srv:/bin/false")
	group := colon("root:x:0:\nnode:x:1000:\nsvc:x:998:\ndocker:x:997:node,svc\nadm:x:4:node")
	for _, c := range []struct {
		spec     string
		uid, gid int
		groups   []int
		home     string
		err      bool
	}{
		{"", 0, 0, []int{0}, "/root", false},
		{"root", 0, 0, []int{0}, "/root", false},
		{"node", 1000, 1000, []int{1000, 997, 4}, "/home/node", false},
		{"1000", 1000, 1000, []int{1000, 997, 4}, "/home/node", false},
		{"1000:1000", 1000, 1000, []int{1000}, "/home/node", false},
		{"svc:docker", 999, 997, []int{997}, "/srv", false},
		{"4242", 4242, 4242, []int{4242}, "/", false},
		{"4242:7", 4242, 7, []int{7}, "/", false},
		{"nobody", 0, 0, nil, "", true},
		{"node:nogroup", 0, 0, nil, "", true},
	} {
		uid, gid, groups, home, err := lookupUserIn(c.spec, passwd, group)
		if c.err {
			if err == nil {
				t.Errorf("%q: want an error, got uid %d", c.spec, uid)
			}
			continue
		}
		if err != nil || uid != c.uid || gid != c.gid || home != c.home || !reflect.DeepEqual(groups, c.groups) {
			t.Errorf("%q: got %d %d %v %q %v, want %d %d %v %q", c.spec, uid, gid, groups, home, err, c.uid, c.gid, c.groups, c.home)
		}
	}
}

func TestMergeEnvAndLookPath(t *testing.T) {
	got := mergeEnv([]string{"PATH=/usr/bin:/bin", "A=1", "B=2"}, []string{"B=3", "C=4", "A=5"})
	if !reflect.DeepEqual(got, []string{"PATH=/usr/bin:/bin", "A=5", "B=3", "C=4"}) {
		t.Fatal(got)
	}
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(d, "noexec"), []byte("x"), 0o644)
	if p, err := lookPath("tool", "/nonexistent:"+d); err != nil || p != filepath.Join(d, "tool") {
		t.Fatal(p, err)
	}
	if _, err := lookPath("noexec", d); err == nil {
		t.Fatal("a non-executable file was accepted")
	}
	if _, err := lookPath("missing", d); err == nil {
		t.Fatal("missing binary accepted")
	}
}
