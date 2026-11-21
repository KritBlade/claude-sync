package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createConversationDatabase writes a small database shaped like an Antigravity conversation: a protobuf
// blob naming the workspace and a text column with a command path.
func createConversationDatabase(t *testing.T, path, workspaceURI, commandPath string) {
	t.Helper()
	database, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrWarn(database, path)
	statements := []string{
		"CREATE TABLE steps (idx integer, step_payload blob, metadata text, PRIMARY KEY (idx))",
		"CREATE TABLE trajectory_metadata_blob (id text DEFAULT 'main', data blob, PRIMARY KEY (id))",
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	workspace := encodeField(7, 2, []byte(workspaceURI))
	if _, err := database.Exec("INSERT INTO trajectory_metadata_blob (id, data) VALUES ('main', ?)", workspace); err != nil {
		t.Fatal(err)
	}
	step := encodeField(1, 2, encodeField(1, 2, []byte(commandPath)))
	if _, err := database.Exec("INSERT INTO steps (idx, step_payload, metadata) VALUES (0, ?, ?)", step, `{"cwd":"`+strings.ReplaceAll(commandPath, `\`, `\\`)+`"}`); err != nil {
		t.Fatal(err)
	}
}

func readSingleBlob(t *testing.T, path, query string) []byte {
	t.Helper()
	database, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrWarn(database, path)
	var value []byte
	if err := database.QueryRow(query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestDatabaseConversionRewritesEveryPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.db")
	createConversationDatabase(t, path, `file:///h%3A/Github/Dev/VectFox`, `H:\Github\Dev\VectFox\src`)
	converter := windowsConverter(t)
	report, err := convertDatabaseFile(path, func(data []byte) []byte { converted, _ := converter.toPortable(data); return converted }, converter.containsLocalRoot)
	if err != nil {
		t.Fatal(err)
	}
	if report.changedValues != 3 || report.unhandledFields != 0 {
		t.Errorf("report = %+v, want 3 changed values and no unhandled fields", report)
	}
	workspace := readSingleBlob(t, path, "SELECT data FROM trajectory_metadata_blob")
	if got := fieldPayload(t, workspace, 7); string(got) != `file://${PORTABLE_PATH:WORK}/VectFox` {
		t.Errorf("workspace = %s", got)
	}
	metadata := readSingleBlob(t, path, "SELECT metadata FROM steps")
	if string(metadata) != `{"cwd":"${PORTABLE_PATH:WORK}/VectFox/src"}` {
		t.Errorf("metadata = %s", metadata)
	}
}

func TestContentHashIgnoresJournalModeAndTracksRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.db")
	createConversationDatabase(t, path, `file:///Users/goten/Github/Dev/VectFox`, `/Users/goten/Github/Dev/VectFox`)
	before, err := databaseContentHash(path)
	if err != nil {
		t.Fatal(err)
	}
	database, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatal(err)
	}
	closeOrWarn(database, path)
	afterJournalChange, err := databaseContentHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterJournalChange != before {
		t.Error("switching the journal mode changed the content hash")
	}
	database, err = openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO steps (idx, step_payload) VALUES (1, x'0a00')"); err != nil {
		t.Fatal(err)
	}
	closeOrWarn(database, path)
	afterNewStep, err := databaseContentHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterNewStep == before {
		t.Error("a new step did not change the content hash")
	}
}

func TestSnapshotIncludesUncheckpointedChanges(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "conversation.db")
	createConversationDatabase(t, path, `file:///Users/goten/x`, `/Users/goten/x`)
	writer, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrWarn(writer, path)
	for _, statement := range []string{"PRAGMA journal_mode = WAL", "PRAGMA wal_autocheckpoint = 0", "INSERT INTO steps (idx, step_payload) VALUES (9, x'0a00')"} {
		if _, err := writer.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	snapshotPath := filepath.Join(directory, "snapshot.db")
	if err := snapshotDatabase(path, snapshotPath); err != nil {
		t.Fatal(err)
	}
	snapshot, err := openDatabase(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrWarn(snapshot, snapshotPath)
	var count int
	if err := snapshot.QueryRow("SELECT count(*) FROM steps WHERE idx = 9").Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot missed the step still in the WAL file (count %d, err %v)", count, err)
	}
}

// TestRealConversationConvertsFromWindowsToMac runs the conversion on a real Windows conversation when
// ANTIGRAVITY_SYNC_SAMPLE_DATABASE names one. Personal data, so it is never committed.
func TestRealConversationConvertsFromWindowsToMac(t *testing.T) {
	samplePath := os.Getenv("ANTIGRAVITY_SYNC_SAMPLE_DATABASE")
	if samplePath == "" {
		t.Skip("set ANTIGRAVITY_SYNC_SAMPLE_DATABASE to a Windows conversation .db to run")
	}
	directory := t.TempDir()
	workingPath := filepath.Join(directory, "conversation.db")
	if err := snapshotDatabase(samplePath, workingPath); err != nil {
		t.Fatal(err)
	}
	windows, mac := windowsConverter(t), macConverter(t)
	toPortable := func(data []byte) []byte { converted, _ := windows.toPortable(data); return converted }
	report, err := convertDatabaseFile(workingPath, toPortable, windows.containsLocalRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("to portable: %+v", report)
	if report.unhandledFields != 0 {
		t.Errorf("%d fields left unconverted", report.unhandledFields)
	}
	unknownTokens := 0
	toLocal := func(data []byte) []byte {
		converted, unknown := mac.toLocal(data)
		unknownTokens += unknown
		return converted
	}
	if _, err := convertDatabaseFile(workingPath, toLocal, containsPortableToken); err != nil {
		t.Fatal(err)
	}
	if unknownTokens != 0 {
		t.Errorf("%d tokens unresolved on the Mac", unknownTokens)
	}
	workspace := readSingleBlob(t, workingPath, "SELECT data FROM trajectory_metadata_blob")
	if !strings.Contains(string(workspace), "file:///Users/goten/Github/Dev/VectFox") || strings.Contains(strings.ToLower(string(workspace)), "h%3a") {
		t.Errorf("workspace record still not on the Mac path")
	}
	database, err := openDatabase(workingPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrWarn(database, workingPath)
	rows, err := database.Query("SELECT step_payload FROM steps")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	remainingWindowsPaths, imagePaths := 0, 0
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if windows.containsLocalRoot(payload) {
			remainingWindowsPaths++
		}
		imagePaths += strings.Count(string(payload), "/Users/goten/.gemini/antigravity/brain/")
	}
	if remainingWindowsPaths != 0 {
		t.Errorf("%d steps still name a mapped Windows folder", remainingWindowsPaths)
	}
	t.Logf("uploaded-image paths now on the Mac: %d", imagePaths)
}
