package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A Windows project folder keeps whichever drive-letter case the launch that
// first created it used, and nothing downstream can tell "C--…" and "c--…"
// apart as directories while every string comparison can. These cover the four
// places that have to agree on one spelling.

func TestCanonicalDriveLetterLowercasesOnlyAWindowsSegment(t *testing.T) {
	cases := map[string]string{
		"H--Github-Dev-app": "h--Github-Dev-app",
		"C--Users-bob":      "c--Users-bob",
		"h--Github-Dev-app": "h--Github-Dev-app",
		"-Users-alice-app":  "-Users-alice-app",
		"${WORK}-app":       "${WORK}-app",
		"Hyphenless":        "Hyphenless",
		"H-single-dash":     "H-single-dash",
		"":                  "",
	}
	for input, want := range cases {
		if got := canonicalDriveLetter(input); got != want {
			t.Errorf("canonicalDriveLetter(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCanonicalRelPathTouchesOnlyTheProjectFolder(t *testing.T) {
	cases := map[string]string{
		"projects/H--Github-Dev-app/s.jsonl": "projects/h--Github-Dev-app/s.jsonl",
		"projects/H--Github-Dev-app":         "projects/h--Github-Dev-app",
		"projects/-Users-alice-app/s.jsonl":  "projects/-Users-alice-app/s.jsonl",
		"history.jsonl":                      "history.jsonl",
		"plans/P--lan.md":                    "plans/P--lan.md",
	}
	for input, want := range cases {
		if got := canonicalRelPath(input); got != want {
			t.Errorf("canonicalRelPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRelPathMappingIgnoresDriveLetterCase(t *testing.T) {
	// Entry and folder disagree on case in each direction.
	for _, configured := range []string{`h:\Github\Dev`, `H:\Github\Dev`} {
		mapper, err := NewPathMapper(`C:\Users\bob`, map[string]string{configured: "WORK"})
		if err != nil {
			t.Fatalf("NewPathMapper(%q): %v", configured, err)
		}
		for _, folder := range []string{"h--Github-Dev-app", "H--Github-Dev-app"} {
			local := "projects/" + folder + "/s.jsonl"
			remote := mapper.NormalizeRelPath(local)
			if remote != "projects/${WORK}-app/s.jsonl" {
				t.Errorf("entry %q, folder %q: NormalizeRelPath = %q", configured, folder, remote)
			}
			back, ok := mapper.ResolveRelPath(remote)
			if !ok || back != "projects/h--Github-Dev-app/s.jsonl" {
				t.Errorf("entry %q, folder %q: ResolveRelPath = %q (ok=%v)", configured, folder, back, ok)
			}
		}
	}
}

// A nested folder under the mapped root whose drive letter was uppercased by
// the launch that created it travelled untranslated before this change. Once
// the case stops mattering it is covered by the entry like any other.
func TestNestedWindowsFolderIsMappedDespiteItsDriveLetterCase(t *testing.T) {
	mapper, err := NewPathMapper(`C:\Users\bob`, map[string]string{`h:\Github\Dev`: "WORK"})
	if err != nil {
		t.Fatal(err)
	}
	remote := mapper.NormalizeRelPath("projects/H--Github-Dev-app-maker-GameLogic/s.jsonl")
	if remote != "projects/${WORK}-app-maker-GameLogic/s.jsonl" {
		t.Errorf("NormalizeRelPath = %q", remote)
	}
	back, ok := mapper.ResolveRelPath(remote)
	if !ok || back != "projects/h--Github-Dev-app-maker-GameLogic/s.jsonl" {
		t.Errorf("ResolveRelPath = %q (ok=%v)", back, ok)
	}
}

// A folder no entry covers travels under its own name, so both devices must
// still agree on which spelling that is.
func TestUnmappedWindowsFolderStillGoesUpCanonically(t *testing.T) {
	mapper, err := NewPathMapper(`C:\Users\bob`, map[string]string{`h:\Github\Dev`: "WORK"})
	if err != nil {
		t.Fatal(err)
	}
	want := "projects/w--Scratch-notes/s.jsonl"
	remote := mapper.NormalizeRelPath("projects/W--Scratch-notes/s.jsonl")
	if remote != want {
		t.Errorf("NormalizeRelPath = %q, want %q", remote, want)
	}
	back, ok := mapper.ResolveRelPath(remote)
	if !ok || back != want {
		t.Errorf("ResolveRelPath = %q (ok=%v), want %q", back, ok, want)
	}
}

func TestContentMatchesEitherDriveLetterCase(t *testing.T) {
	mapper, err := NewPathMapper(`C:\Users\bob`, map[string]string{`C:\Derek\Github\Dev`: "WORK"})
	if err != nil {
		t.Fatal(err)
	}
	const relPath = "projects/whatever/s.jsonl"
	want := `{"cwd":"${WORK}/app"}`
	for _, written := range []string{
		`{"cwd":"C:\\Derek\\Github\\Dev\\app"}`,
		`{"cwd":"c:\\Derek\\Github\\Dev\\app"}`,
	} {
		if got := string(mapper.NormalizeContent(relPath, []byte(written))); got != want {
			t.Errorf("NormalizeContent(%s) = %s, want %s", written, got, want)
		}
	}
}

func TestLocalWalkRecordsTheCanonicalSpelling(t *testing.T) {
	claudeDir := t.TempDir()
	folder := filepath.Join(claudeDir, "projects", "H--Github-Dev-app")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "s.jsonl"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}

	files, err := GetLocalFiles(claudeDir, []string{"projects"})
	if err != nil {
		t.Fatal(err)
	}
	if _, found := files["projects/h--Github-Dev-app/s.jsonl"]; !found {
		keys := make([]string, 0, len(files))
		for key := range files {
			keys = append(keys, key)
		}
		t.Errorf("walk did not record the canonical key; got %v", keys)
	}
}

// The trap this change exists to close: a key recorded under the old spelling
// against a folder the walk now reports canonically reads as a new upload AND a
// deletion of the same remote object, which uploads the file and then deletes it.
func TestStateKeysSurviveTheCaseChangeWithoutChurn(t *testing.T) {
	claudeDir := t.TempDir()
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	folder := filepath.Join(claudeDir, "projects", "H--Github-Dev-app")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(folder, "s.jsonl")
	contents := []byte("{\"a\":1}\n")
	if err := os.WriteFile(sessionPath, contents, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := HashFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}

	// State as an older build wrote it: the uppercase spelling.
	oldKey := "projects/H--Github-Dev-app/s.jsonl"
	previous := &SyncState{
		Files: map[string]*FileState{
			oldKey: {Path: oldKey, Hash: hash, Size: info.Size(), ModTime: info.ModTime()},
		},
		DeviceID: "TEST",
	}
	raw, err := json.MarshalIndent(previous, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0600); err != nil {
		t.Fatal(err)
	}

	state, err := loadStateFromPath(statePath)
	if err != nil {
		t.Fatal(err)
	}

	canonicalKey := "projects/h--Github-Dev-app/s.jsonl"
	if _, found := state.Files[canonicalKey]; !found {
		t.Fatalf("state key was not rewritten; have %v", state.Files)
	}
	if _, found := state.Files[oldKey]; found {
		t.Error("the old key is still present")
	}
	if entry := state.Files[canonicalKey]; entry != nil && entry.Path != canonicalKey {
		t.Errorf("entry Path = %q, want %q", entry.Path, canonicalKey)
	}

	if _, err := os.Stat(statePath + ".bak-drive-letter"); err != nil {
		t.Errorf("the previous state was not kept: %v", err)
	}

	changes, err := state.DetectChanges(claudeDir, []string{"projects"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("expected no work after the rewrite, got %d change(s): %+v", len(changes), changes)
	}

	// A second load has nothing left to do.
	reloaded, err := loadStateFromPath(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := reloaded.Files[canonicalKey]; !found {
		t.Error("the rewrite did not persist")
	}
}

func TestStateRewriteKeepsTheCanonicalEntryOnACollision(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	canonicalKey := "projects/h--Github-Dev-app/s.jsonl"
	uppercaseKey := "projects/H--Github-Dev-app/s.jsonl"
	previous := &SyncState{
		Files: map[string]*FileState{
			canonicalKey: {Path: canonicalKey, Hash: "canonical", Size: 1},
			uppercaseKey: {Path: uppercaseKey, Hash: "uppercase", Size: 2},
		},
		DeviceID: "TEST",
	}
	raw, err := json.MarshalIndent(previous, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0600); err != nil {
		t.Fatal(err)
	}

	state, err := loadStateFromPath(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Files) != 1 {
		t.Fatalf("expected one entry, got %d", len(state.Files))
	}
	if entry := state.Files[canonicalKey]; entry == nil || entry.Hash != "canonical" {
		t.Errorf("the canonical entry was not kept: %+v", state.Files[canonicalKey])
	}
}

func TestStateRewriteLeavesACanonicalStateAlone(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")

	key := "projects/-Users-alice-app/s.jsonl"
	previous := &SyncState{
		Files:    map[string]*FileState{key: {Path: key, Hash: "x", Size: 1}},
		DeviceID: "TEST",
	}
	raw, err := json.MarshalIndent(previous, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadStateFromPath(statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath + ".bak-drive-letter"); err == nil {
		t.Error("a backup was written for a state that needed no rewrite")
	}
}

// A second rewrite must not replace the state as it stood before the first one.
func TestStateRewriteKeepsTheOriginalBackup(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")
	backupPath := statePath + ".bak-drive-letter"

	writeState := func(key string) {
		raw, err := json.MarshalIndent(&SyncState{
			Files:    map[string]*FileState{key: {Path: key, Hash: "x", Size: 1}},
			DeviceID: "TEST",
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}

	writeState("projects/H--Github-Dev-app/first.jsonl")
	if _, err := loadStateFromPath(statePath); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("no backup after the first rewrite: %v", err)
	}

	// A later state that still needs rewriting must leave that backup alone.
	writeState("projects/H--Github-Dev-app/second.jsonl")
	if _, err := loadStateFromPath(statePath); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Error("the original backup was overwritten by a later rewrite")
	}
}
