package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// syncedFileRecord is what this device knew about one file at its last sync. LocalHash identifies the local
// content (rows for a conversation database); LocalSize and LocalModifiedAt let an unchanged file skip
// rehashing. RemoteETag identifies the cloud copy it was synced with.
type syncedFileRecord struct {
	LocalHash       string    `json:"local_hash"`
	LocalSize       int64     `json:"local_size"`
	LocalModifiedAt int64     `json:"local_modified_at"`
	RemoteETag      string    `json:"remote_etag"`
	SyncedAt        time.Time `json:"synced_at"`
}

type syncState struct {
	path  string
	Files map[string]syncedFileRecord `json:"files"`
}

// loadSyncState reads the state file, or starts empty when there is none yet. A file that cannot be parsed
// is an error rather than a fresh start: starting fresh would treat every file as never synced.
func loadSyncState(path string) (*syncState, error) {
	state := &syncState{path: path, Files: map[string]syncedFileRecord{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("parse %s: %w (move it aside to start over; every file is then compared with the cloud again)", path, err)
	}
	if state.Files == nil {
		state.Files = map[string]syncedFileRecord{}
	}
	return state, nil
}

// save writes the state to a temporary file and renames it over the old one, so a crash never leaves half a
// state file behind.
func (state *syncState) save() error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(state.path), 0o700); err != nil {
		return err
	}
	temporary := state.path + ".writing"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, state.path)
}
