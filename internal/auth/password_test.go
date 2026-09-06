package auth

import (
	"strings"
	"testing"
)

func TestPasswordHashRoundtrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash = %q", h)
	}
	ok, err := VerifyPassword("correct horse battery staple", h)
	if err != nil || !ok {
		t.Fatalf("verify correct = %v, %v", ok, err)
	}
	ok, err = VerifyPassword("wrong", h)
	if err != nil || ok {
		t.Fatalf("verify wrong = %v, %v", ok, err)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Acme Corp":      "acme-corp",
		"  Weird__Name ": "weird-name",
		"!!!":            "item",
		"Ünïcode":        "ncode",
		"a.b/c":          "a-bc",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAPITokenShape(t *testing.T) {
	full, prefix, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidAPITokenShape(full) {
		t.Fatalf("shape %q invalid", full)
	}
	if !strings.HasPrefix(full, prefix) {
		t.Fatalf("prefix %q not part of %q", prefix, full)
	}
	if ValidAPITokenShape("nope") {
		t.Fatal("bad shape accepted")
	}
}
