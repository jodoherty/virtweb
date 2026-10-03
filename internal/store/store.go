// Package store persists the list of known libvirt servers as a JSON file.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/jodoherty/virtweb/internal/virt"
)

// Store is a file-backed, concurrency-safe store of server definitions.
type Store struct {
	mu   sync.Mutex
	path string
	srvs []virt.Server
}

// New loads the store from path, creating it (and its directory) if absent.
func New(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read store: %w", err)
		}
	} else {
		if err := json.Unmarshal(data, &s.srvs); err != nil {
			return nil, fmt.Errorf("parse store: %w", err)
		}
	}
	return s, nil
}

// List returns a copy of all servers.
func (s *Store) List() []virt.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]virt.Server, len(s.srvs))
	copy(out, s.srvs)
	return out
}

// Get returns the server with the given id.
func (s *Store) Get(id string) (virt.Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range s.srvs {
		if sv.ID == id {
			return sv, nil
		}
	}
	return virt.Server{}, fmt.Errorf("server %q not found", id)
}

// Validate checks a server definition for sanity.
func Validate(srv virt.Server) error {
	if srv.Name == "" {
		return errors.New("name is required")
	}
	if srv.User == "" {
		return errors.New("user is required")
	}
	if srv.Host == "" || len(srv.Host) > 255 {
		return errors.New("host is required")
	}
	if srv.Port < 0 || srv.Port > 65535 {
		return errors.New("port out of range")
	}
	if srv.URI == "" {
		srv.URI = "qemu:///system"
	}
	return nil
}

// Add validates, assigns an id, stores and persists a new server.
// It fails if a server with the same name already exists.
func (s *Store) Add(srv virt.Server) (virt.Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	srv.ID = ""
	if err := Validate(srv); err != nil {
		return virt.Server{}, err
	}
	if srv.URI == "" {
		srv.URI = "qemu:///system"
	}
	for _, ex := range s.srvs {
		if ex.Name == srv.Name {
			return virt.Server{}, fmt.Errorf("server %q already exists", srv.Name)
		}
	}
	id, err := randomID()
	if err != nil {
		return virt.Server{}, err
	}
	srv.ID = id
	s.srvs = append(s.srvs, srv)
	if err := s.persist(); err != nil {
		s.srvs = s.srvs[:len(s.srvs)-1]
		return virt.Server{}, err
	}
	return srv, nil
}

// randomID returns a new random 16-hex-character identifier.
func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Update replaces the definition of the server with the given id,
// preserving its id. It fails if no such server exists, the new
// definition is invalid, or another server already has the new name.
func (s *Store) Update(id string, srv virt.Server) (virt.Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.srvs {
		if s.srvs[i].ID != id {
			continue
		}
		srv.ID = id
		if err := Validate(srv); err != nil {
			return virt.Server{}, err
		}
		if srv.URI == "" {
			srv.URI = "qemu:///system"
		}
		for _, other := range s.srvs {
			if other.ID != id && other.Name == srv.Name {
				return virt.Server{}, fmt.Errorf("server %q already exists", srv.Name)
			}
		}
		s.srvs[i] = srv
		if err := s.persist(); err != nil {
			return virt.Server{}, err
		}
		return srv, nil
	}
	return virt.Server{}, fmt.Errorf("server %q not found", id)
}

// Remove deletes the server with the given id and persists.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sv := range s.srvs {
		if sv.ID == id {
			s.srvs = append(s.srvs[:i], s.srvs[i+1:]...)
			return s.persist()
		}
	}
	return fmt.Errorf("server %q not found", id)
}

func (s *Store) persist() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.srvs, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
