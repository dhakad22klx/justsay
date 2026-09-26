// Package credentials keeps what an integration needs in order to be trusted
// again on the next run: a bot token, the identity that was paired with it. One
// JSON file holds them all, a key per integration, so adding a service does not
// add a file to find, chmod and gitignore.
//
// Nothing here knows what any integration's entry contains. A caller hands over
// a value to store and a value to decode into, which keeps the shape of a
// credential where the integration that understands it lives, and keeps this
// package about the file: where it is, who may read it, and not losing the
// entries it does not recognise.
package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultPath is where the file lives, relative to where the agent was started,
// alongside the session transcripts it is already writing.
const DefaultPath = "credentials.json"

// fileMode keeps the file readable by its owner alone. It is enforced on every
// save rather than only at creation, so a file that was loosened by hand
// tightens again the next time anything is written.
const fileMode = 0o600

// Store is the credentials file, held open only long enough to read or write.
//
// Unrecognised keys are carried through untouched. That is the whole reason the
// document is kept as raw JSON per key instead of a struct: a store written by
// a newer build, or by an integration this one has never heard of, must survive
// a save made here.
type Store struct {
	path string
	doc  map[string]json.RawMessage
}

// Open reads the file at path, or returns an empty store when there is nothing
// there yet. A missing file is the ordinary first run and not a failure; a file
// that exists but cannot be read or parsed is, because overwriting it would
// throw away credentials the user still has.
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath
	}

	store := &Store{path: path, doc: map[string]json.RawMessage{}}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	// A file truncated to nothing reads as an empty store rather than as
	// malformed JSON, since that is what an interrupted write leaves behind.
	if len(bytes.TrimSpace(raw)) == 0 {
		return store, nil
	}

	if err := json.Unmarshal(raw, &store.doc); err != nil {
		return nil, fmt.Errorf("cannot parse %s: %w", path, err)
	}

	return store, nil
}

// Path is the file being read and written, which is safe to show the user. The
// contents are not.
func (s *Store) Path() string { return s.path }

// Get decodes the entry for name into into. The boolean says whether there was
// an entry at all, which is how a caller tells "never paired" from "paired with
// something I could not read".
func (s *Store) Get(name string, into any) (bool, error) {
	raw, ok := s.doc[name]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return false, nil
	}

	if err := json.Unmarshal(raw, into); err != nil {
		return false, fmt.Errorf("cannot read the %s entry in %s: %w", name, s.path, err)
	}

	return true, nil
}

// Set replaces the entry for name in memory. Nothing reaches disk until Save,
// so a half-built credential cannot be left behind by an abandoned pairing.
func (s *Store) Set(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cannot encode the %s entry: %w", name, err)
	}

	s.doc[name] = raw

	return nil
}

// Save writes the whole document back.
//
// The write goes to a temporary file in the same directory and is renamed over
// the target, because a failure partway through a direct write would leave a
// file that parses as neither the old credentials nor the new ones. The
// temporary file is created at fileMode, and the rename carries that mode onto
// the result, which is what tightens a file that had been loosened.
func (s *Store) Save() (saveErr error) {
	encoded, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode %s: %w", s.path, err)
	}
	encoded = append(encoded, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}

	temp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot write near %s: %w", s.path, err)
	}
	tempPath := temp.Name()

	// From here on the temporary file is removed on every path out but the
	// successful rename, so a failed save does not litter the directory with
	// files holding a token.
	defer func() {
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			saveErr = errors.Join(saveErr, fmt.Errorf("cannot remove temporary credentials file: %w", err))
		}
	}()

	if err := temp.Chmod(fileMode); err != nil {
		_ = temp.Close() // Preserve the permission error; deferred removal still runs.
		return fmt.Errorf("cannot restrict permissions on %s: %w", s.path, err)
	}

	if _, err := temp.Write(encoded); err != nil {
		_ = temp.Close() // Preserve the write error; deferred removal still runs.
		return fmt.Errorf("cannot write %s: %w", s.path, err)
	}

	if err := temp.Close(); err != nil {
		return fmt.Errorf("cannot finish writing %s: %w", s.path, err)
	}

	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("cannot replace %s: %w", s.path, err)
	}

	return nil
}
