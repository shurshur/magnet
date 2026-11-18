package magnetutil

import (
	"errors"
	"strings"
	"testing"

	"github.com/shurshur/magnet/internal/model"
)

const (
	testV1 = "0123456789abcdef0123456789abcdef01234567"
	testV2 = "caf1e1c30e81cb361b9ee167c4aa64228a7fa4fa9f6105232b28ad099f3a302e"
)

func TestParseV1(t *testing.T) {
	parsed, err := Parse("  magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567&dn=Test  ")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PrimaryID != testV1 || parsed.InfoHashV1 != testV1 || parsed.InfoHashV2 != "" {
		t.Fatalf("parsed = %+v", parsed)
	}
	if parsed.Version != model.VersionV1 {
		t.Fatalf("version = %q", parsed.Version)
	}
	if parsed.Link != "magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567&dn=Test" {
		t.Fatalf("link = %q", parsed.Link)
	}
}

func TestParseV2(t *testing.T) {
	parsed, err := Parse("magnet:?xt=urn:btmh:1220" + testV2 + "&dn=v2")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PrimaryID != testV2 || parsed.InfoHashV2 != testV2 || parsed.InfoHashV1 != "" {
		t.Fatalf("parsed = %+v", parsed)
	}
	if parsed.Version != model.VersionV2 {
		t.Fatalf("version = %q", parsed.Version)
	}
}

func TestParseHybrid(t *testing.T) {
	parsed, err := Parse("magnet:?xt=urn:btih:" + testV1 + "&xt=urn:btmh:1220" + testV2)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PrimaryID != testV1 {
		t.Fatalf("primary = %q", parsed.PrimaryID)
	}
	if parsed.InfoHashV1 != testV1 || parsed.InfoHashV2 != testV2 {
		t.Fatalf("parsed = %+v", parsed)
	}
	if parsed.Version != model.VersionHybrid {
		t.Fatalf("version = %q", parsed.Version)
	}
}

func TestParseBase32(t *testing.T) {
	parsed, err := Parse("magnet:?xt=urn:btih:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.PrimaryID) != 40 {
		t.Fatalf("id = %q", parsed.PrimaryID)
	}
}

func TestParseErrors(t *testing.T) {
	for _, link := range []string{"", "http://example.com", "magnet:?dn=no-hash", "magnet:?xt=urn:btih:xyz"} {
		if _, err := Parse(link); err == nil {
			t.Errorf("expected error for %q", link)
		}
	}
	if _, err := Parse("   "); !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}
}

func TestNormalizeID(t *testing.T) {
	id, err := NormalizeID("  0123456789ABCDEF0123456789ABCDEF01234567 ")
	if err != nil {
		t.Fatal(err)
	}
	if id != testV1 {
		t.Fatalf("id = %q", id)
	}

	id, err = NormalizeID(strings.ToUpper(testV2))
	if err != nil {
		t.Fatal(err)
	}
	if id != testV2 {
		t.Fatalf("v2 id = %q", id)
	}

	id, err = NormalizeID("1220" + testV2)
	if err != nil {
		t.Fatal(err)
	}
	if id != testV2 {
		t.Fatalf("btmh id = %q", id)
	}

	for _, bad := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("a", 39), strings.Repeat("a", 63)} {
		if _, err := NormalizeID(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
