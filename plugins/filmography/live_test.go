//go:build live

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
)

// go test -tags live -run Live: needs ~/.tmdb-key and ~/.silo-key.
func TestLivePerson(t *testing.T) {
	home, _ := os.UserHomeDir()
	tk, _ := os.ReadFile(home + "/.tmdb-key")
	sk, _ := os.ReadFile(home + "/.silo-key")
	a := newApp(hclog.NewNullLogger())
	a.configure(entries(map[string]map[string]any{
		"tmdb": {"api_key": strings.TrimSpace(string(tk))},
		"silo": {"api_key": strings.TrimSpace(string(sk)), "base_url": "http://localhost:8098"},
	}))
	p, err := a.person(context.Background(), "31")
	if err != nil {
		t.Fatal(err)
	}
	lib := 0
	for _, c := range p.Credits {
		if c.ContentID != "" {
			lib++
			t.Logf("in library: %s %s -> %s", c.Media, c.Title, c.ContentID)
		}
	}
	t.Logf("%s: %d credits, %d images, %d links, %d in library, silo person %s, err %q", p.Name, len(p.Credits), len(p.Images), len(p.Links), lib, p.SiloPersonID, p.LibraryError)
	if lib == 0 || p.LibraryError != "" {
		t.Fatal("expected library matches")
	}
}
