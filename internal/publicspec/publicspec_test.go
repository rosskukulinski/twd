package publicspec

import (
	"net/url"
	"testing"
)

func TestValidatePatterns(t *testing.T) {
	good := []string{"/*", "/webhooks/*", "/oauth/callback", "/a/b/c", "/mcp"}
	for _, p := range good {
		s := Spec{Routes: []string{p}}
		if err := s.Validate(); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	bad := []string{"", "webhooks", "/a/*/b", "/a*", "/a/", "//a", "/a/../b", "/a/./b", "/_twd/*", "/_twd", "/_twd/o/X/y", "/a?b", "/a#b", "/a%2fb", "/a b", "/*/x", "*"}
	for _, p := range bad {
		s := Spec{Routes: []string{p}}
		if err := s.Validate(); err == nil {
			t.Errorf("%q: accepted", p)
		}
	}
	if err := (&Spec{}).Validate(); err == nil {
		t.Error("empty routes accepted")
	}
	if err := (&Spec{Routes: []string{"/a"}, Upgrade: []string{"/b"}}).Validate(); err == nil {
		t.Error("upgrade outside routes accepted")
	}
	if err := (&Spec{Routes: []string{"/a"}, Limits: &Limits{RPS: 5000}}).Validate(); err == nil {
		t.Error("rps above ceiling accepted")
	}
	if err := (&Spec{Routes: []string{"/a"}, Limits: &Limits{MaxBodyBytes: -1}}).Validate(); err == nil {
		t.Error("negative limit accepted")
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, p string
		want   bool
	}{
		{"/*", "/", true}, {"/*", "/x/y", true},
		{"/webhooks/*", "/webhooks", true}, {"/webhooks/*", "/webhooks/", true}, {"/webhooks/*", "/webhooks/github", true},
		{"/webhooks/*", "/webhooksx", false}, {"/webhooks/*", "/", false},
		{"/oauth/callback", "/oauth/callback", true}, {"/oauth/callback", "/oauth/callback/x", false}, {"/oauth/callback", "/oauth/callback/", false},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.p); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pat, c.p, got, c.want)
		}
	}
}

func TestCanonicalPath(t *testing.T) {
	ok := []string{"/", "/a", "/a/", "/a/b", "/a/b?x=1"}
	for _, s := range ok {
		u, _ := url.Parse(s)
		if _, err := CanonicalPath(u); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := []string{"/a/../b", "/a//b", "/a/./b", "/a%2F..%2Fb", "/a%5cb", "/_twd/o/Room/x", "/_twd", "/x/%2e%2e/y"}
	for _, s := range bad {
		u, err := url.Parse(s)
		if err != nil {
			continue
		}
		if p, err := CanonicalPath(u); err == nil {
			t.Errorf("%q: accepted as %q", s, p)
		}
	}
}

func TestClassSpec(t *testing.T) {
	if err := (&ClassSpec{Create: true}).Validate("Room"); err == nil {
		t.Error("create without maxObjects accepted")
	}
	if err := (&ClassSpec{Create: true, MaxObjects: 1001}).Validate("Room"); err == nil {
		t.Error("maxObjects above ceiling accepted")
	}
	if err := (&ClassSpec{MaxObjects: 3}).Validate("Room"); err == nil {
		t.Error("maxObjects without create accepted")
	}
	if err := (&ClassSpec{Create: true, MaxObjects: 10}).Validate("Room"); err != nil {
		t.Error(err)
	}
}

func TestEffective(t *testing.T) {
	var l *Limits
	if l.Effective() != Defaults {
		t.Fatal("nil limits != defaults")
	}
	e := (&Limits{RPS: 5}).Effective()
	if e.RPS != 5 || e.Burst != Defaults.Burst {
		t.Fatalf("%+v", e)
	}
}
