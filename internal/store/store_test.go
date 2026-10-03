package store

import (
	"os"
	"path/filepath"
	"testing"

	"git.theodohertyfamily.com/virtweb/internal/virt"
)

func TestAddListGetRemove(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "servers.json"))
	if err != nil {
		t.Fatal(err)
	}

	srv, err := s.Add(virt.Server{Name: "alpha", User: "bob", Host: "alpha.example", Port: 22})
	if err != nil {
		t.Fatal(err)
	}
	if srv.ID == "" {
		t.Fatal("expected an id to be assigned")
	}
	if srv.URI != "qemu:///system" {
		t.Fatalf("expected default URI, got %q", srv.URI)
	}

	if got := s.List(); len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("List = %+v", got)
	}

	got, err := s.Get(srv.ID)
	if err != nil || got.Host != "alpha.example" {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	// duplicate name rejected
	if _, err := s.Add(virt.Server{Name: "alpha", User: "bob", Host: "x"}); err == nil {
		t.Fatal("expected duplicate name error")
	}

	if err := s.Remove(srv.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatal("expected empty list after remove")
	}
	if err := s.Remove(srv.ID); err == nil {
		t.Fatal("expected error removing unknown server")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.json")
	s, _ := New(path)
	if _, err := s.Add(virt.Server{Name: "beta", User: "u", Host: "10.0.0.1", Port: 2222}); err != nil {
		t.Fatal(err)
	}

	// fresh instance loads the persisted data
	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.List()
	if len(got) != 1 || got[0].Name != "beta" || got[0].Port != 2222 {
		t.Fatalf("reloaded store = %+v", got)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		srv  virt.Server
		want bool
	}{
		{"ok", virt.Server{Name: "n", User: "u", Host: "h"}, true},
		{"no name", virt.Server{User: "u", Host: "h"}, false},
		{"no user", virt.Server{Name: "n", Host: "h"}, false},
		{"no host", virt.Server{Name: "n", User: "u"}, false},
		{"bad port", virt.Server{Name: "n", User: "u", Host: "h", Port: 99999}, false},
	}
	for _, tc := range cases {
		err := Validate(tc.srv)
		if (err == nil) != tc.want {
			t.Errorf("%s: Validate = %v, want ok=%v", tc.name, err, tc.want)
		}
	}
}

func TestCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil {
		t.Fatal("expected error for corrupt store file")
	}
}
