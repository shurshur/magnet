package magnetutil

import (
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	id, canonical, err := Parse("  magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567&dn=Test  ")
	if err != nil {
		t.Fatal(err)
	}
	if id != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("id = %q", id)
	}
	if canonical != "magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567&dn=Test" {
		t.Fatalf("canonical = %q", canonical)
	}
}

func TestParseBase32(t *testing.T) {
	id, _, err := Parse("magnet:?xt=urn:btih:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 40 {
		t.Fatalf("id = %q", id)
	}
}

func TestParseErrors(t *testing.T) {
	for _, link := range []string{"", "http://example.com", "magnet:?dn=no-hash", "magnet:?xt=urn:btih:xyz"} {
		if _, _, err := Parse(link); err == nil {
			t.Errorf("expected error for %q", link)
		}
	}
	if _, _, err := Parse("   "); !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}
}

func TestNormalizeID(t *testing.T) {
	id, err := NormalizeID("  0123456789ABCDEF0123456789ABCDEF01234567 ")
	if err != nil {
		t.Fatal(err)
	}
	if id != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("id = %q", id)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("a", 39)} {
		if _, err := NormalizeID(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
