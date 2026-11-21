package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// memoryStorage is an in-memory bucket. Each upload gets a new ETag, as a real bucket gives a new one to every
// encrypted upload.
type memoryStorage struct {
	lock    sync.Mutex
	objects map[string][]byte
	etags   map[string]string
	uploads int
}

func newMemoryStorage() *memoryStorage {
	return &memoryStorage{objects: map[string][]byte{}, etags: map[string]string{}}
}

func (store *memoryStorage) Upload(_ context.Context, key string, data []byte) error {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.uploads++
	store.objects[key] = append([]byte(nil), data...)
	store.etags[key] = fmt.Sprintf(`"etag-%d"`, store.uploads)
	return nil
}

func (store *memoryStorage) Download(_ context.Context, key string) ([]byte, error) {
	store.lock.Lock()
	defer store.lock.Unlock()
	data, isStored := store.objects[key]
	if !isStored {
		return nil, fmt.Errorf("no object %s", key)
	}
	return append([]byte(nil), data...), nil
}

func (store *memoryStorage) Delete(_ context.Context, key string) error {
	store.lock.Lock()
	defer store.lock.Unlock()
	delete(store.objects, key)
	delete(store.etags, key)
	return nil
}

func (store *memoryStorage) DeleteBatch(ctx context.Context, keys []string) error {
	for _, key := range keys {
		if err := store.Delete(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (store *memoryStorage) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	store.lock.Lock()
	defer store.lock.Unlock()
	var objects []storage.ObjectInfo
	for key, data := range store.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, storage.ObjectInfo{Key: key, Size: int64(len(data)), ETag: store.etags[key], LastModified: time.Now()})
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

func (store *memoryStorage) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	store.lock.Lock()
	defer store.lock.Unlock()
	if _, isStored := store.objects[key]; !isStored {
		return nil, fmt.Errorf("no object %s", key)
	}
	return &storage.ObjectInfo{Key: key, ETag: store.etags[key]}, nil
}

func (store *memoryStorage) BucketExists(context.Context) (bool, error) { return true, nil }

type testDevice struct {
	engine *syncEngine
	output *bytes.Buffer
}

func newTestDevice(t *testing.T, store storage.Storage, encryptor *crypto.Encryptor, paths *pathConverter) *testDevice {
	t.Helper()
	stateDirectory := t.TempDir()
	state, err := loadSyncState(filepath.Join(stateDirectory, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	return &testDevice{output: output, engine: &syncEngine{
		geminiDirectory: t.TempDir(), stateDirectory: stateDirectory, store: store, encryptor: encryptor,
		paths: paths, state: state, out: output, now: time.Now,
	}}
}

func (device *testDevice) path(relativePath string) string {
	return device.engine.localPath(relativePath)
}

func (device *testDevice) write(t *testing.T, relativePath, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(device.path(relativePath)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(device.path(relativePath), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (device *testDevice) read(t *testing.T, relativePath string) string {
	t.Helper()
	data, err := os.ReadFile(device.path(relativePath))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (device *testDevice) pull(t *testing.T, options runOptions) {
	t.Helper()
	device.output.Reset()
	summary, err := device.engine.pull(context.Background(), options)
	if err != nil || summary.failures != 0 {
		t.Fatalf("pull: %v (failures %d)\n%s", err, summary.failures, device.output)
	}
}

func (device *testDevice) push(t *testing.T, options runOptions) {
	t.Helper()
	device.output.Reset()
	summary, err := device.engine.push(context.Background(), options)
	if err != nil || summary.failures != 0 {
		t.Fatalf("push: %v (failures %d)\n%s", err, summary.failures, device.output)
	}
}

const (
	conversationID     = "09190687-bacb-47b4-b2ad-260f078f9240"
	conversationPath   = "antigravity/conversations/" + conversationID + ".db"
	notesPath          = "antigravity/brain/" + conversationID + "/implementation_plan.md"
	imagePath          = "antigravity/brain/" + conversationID + "/.user_uploaded/media_1.jpg"
	annotationPath     = "antigravity/annotations/" + conversationID + ".pbtxt"
	projectPath        = "config/projects/df1ef2d2-89fc-4b37-96b2-a0b9bd601d14.json"
	imageBytes         = "\xff\xd8\xff\x00binary H:\\Github\\Dev\\x"
	windowsProjectJSON = `{"id":"df1ef2d2","name":"VectFox","projectResources":{"resources":[{"folderUri":"file:///h%3A/Github/Dev/VectFox"}]}}`
	macProjectJSON     = `{"id":"df1ef2d2","name":"VectFox","projectResources":{"resources":[{"folderUri":"file:///Users/goten/Github/Dev/VectFox"}]}}`
)

func newTwoDevices(t *testing.T) (*memoryStorage, *testDevice, *testDevice) {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "age-key.txt")
	if err := crypto.GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	encryptor, err := crypto.NewEncryptor(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStorage()
	return store, newTestDevice(t, store, encryptor, windowsConverter(t)), newTestDevice(t, store, encryptor, macConverter(t))
}

// seedWindowsConversation lays out one conversation as Antigravity writes it on Windows, plus files that are
// never synced.
func seedWindowsConversation(t *testing.T, windows *testDevice) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(windows.path(conversationPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	createConversationDatabase(t, windows.path(conversationPath), `file:///h%3A/Github/Dev/VectFox`, `H:\Github\Dev\VectFox\src`)
	windows.write(t, notesPath, "Plan for H:\\Github\\Dev\\VectFox\\a.md\n")
	windows.write(t, imagePath, imageBytes)
	windows.write(t, annotationPath, `title:"Gemini equivalent of CLAUDE.md"`)
	windows.write(t, projectPath, windowsProjectJSON)
	windows.write(t, "antigravity/conversations/"+conversationID+".db-wal", "side file")
	windows.write(t, "config/projects/default-cli-project.json", `{"id":"default-cli-project"}`)
	windows.write(t, "antigravity/browser_recordings/x.webm", "not synced")
}

func TestConversationTravelsFromWindowsToMacWithMacPaths(t *testing.T) {
	store, windows, mac := newTwoDevices(t)
	seedWindowsConversation(t, windows)
	windows.push(t, runOptions{})

	var keys []string
	for key := range store.objects {
		keys = append(keys, strings.TrimPrefix(key, remotePrefix))
	}
	sort.Strings(keys)
	wantKeys := []string{annotationPath + ".age", notesPath + ".age", imagePath + ".age", conversationPath + ".age", projectPath + ".age"}
	sort.Strings(wantKeys)
	if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("cloud holds %v, want %v", keys, wantKeys)
	}

	mac.pull(t, runOptions{})
	workspace := readSingleBlob(t, mac.path(conversationPath), "SELECT data FROM trajectory_metadata_blob")
	if got := fieldPayload(t, workspace, 7); string(got) != "file:///Users/goten/Github/Dev/VectFox" {
		t.Errorf("workspace on the Mac = %s", got)
	}
	if got := mac.read(t, notesPath); got != "Plan for /Users/goten/Github/Dev/VectFox/a.md\n" {
		t.Errorf("notes on the Mac = %q", got)
	}
	if got := mac.read(t, imagePath); got != imageBytes {
		t.Error("binary image changed in transit")
	}
	if got := mac.read(t, projectPath); got != macProjectJSON {
		t.Errorf("project on the Mac = %s", got)
	}

	mac.pull(t, runOptions{})
	if !strings.Contains(mac.output.String(), "nothing to pull") {
		t.Errorf("second pull should find nothing:\n%s", mac.output)
	}
	mac.push(t, runOptions{})
	if !strings.Contains(mac.output.String(), "nothing to push") {
		t.Errorf("pulled files must not be pushed back:\n%s", mac.output)
	}
}

func TestEditsTravelBackToWindowsWithWindowsPaths(t *testing.T) {
	_, windows, mac := newTwoDevices(t)
	seedWindowsConversation(t, windows)
	windows.push(t, runOptions{})
	mac.pull(t, runOptions{})

	mac.write(t, notesPath, mac.read(t, notesPath)+"Next: /Users/goten/Github/Dev/VectFox/b.md\n")
	mac.push(t, runOptions{})
	windows.pull(t, runOptions{})
	want := "Plan for H:/Github/Dev/VectFox/a.md\nNext: H:/Github/Dev/VectFox/b.md\n"
	if got := windows.read(t, notesPath); got != want {
		t.Errorf("notes back on Windows = %q, want %q", got, want)
	}
	windows.push(t, runOptions{})
	if !strings.Contains(windows.output.String(), "nothing to push") {
		t.Errorf("a pulled edit must not be pushed back:\n%s", windows.output)
	}
}

func TestEditsOnBothSidesKeepTheLocalCopyAndSaveTheCloudsCopy(t *testing.T) {
	_, windows, mac := newTwoDevices(t)
	seedWindowsConversation(t, windows)
	windows.push(t, runOptions{})
	mac.pull(t, runOptions{})

	mac.write(t, notesPath, "Mac edit\n")
	mac.push(t, runOptions{})
	windows.write(t, notesPath, "Windows edit\n")

	windows.push(t, runOptions{})
	if !strings.Contains(windows.output.String(), "pull first") {
		t.Fatalf("push over a newer cloud copy should be skipped:\n%s", windows.output)
	}
	windows.pull(t, runOptions{})
	if got := windows.read(t, notesPath); got != "Windows edit\n" {
		t.Errorf("local copy was replaced: %q", got)
	}
	conflictCopies, err := filepath.Glob(filepath.Join(windows.engine.stateDirectory, "conflicts", "*", filepath.FromSlash(notesPath)))
	if err != nil || len(conflictCopies) != 1 {
		t.Fatalf("conflict copies = %v (err %v)", conflictCopies, err)
	}
	if saved, _ := os.ReadFile(conflictCopies[0]); string(saved) != "Mac edit\n" {
		t.Errorf("saved cloud copy = %q", saved)
	}
	windows.push(t, runOptions{})
	mac.pull(t, runOptions{})
	if got := mac.read(t, notesPath); got != "Windows edit\n" {
		t.Errorf("after resolving, the Mac has %q", got)
	}
}

func TestDeletionsMoveToTheTrashAndRespectTheGuards(t *testing.T) {
	store, windows, mac := newTwoDevices(t)
	seedWindowsConversation(t, windows)
	for i := range 4 {
		windows.write(t, fmt.Sprintf("antigravity/brain/%s/extra-%d.md", conversationID, i), "extra")
	}
	windows.push(t, runOptions{})
	mac.pull(t, runOptions{})

	if err := os.Remove(windows.path(notesPath)); err != nil {
		t.Fatal(err)
	}
	windows.push(t, runOptions{})
	mac.pull(t, runOptions{})
	if _, err := os.Stat(mac.path(notesPath)); !os.IsNotExist(err) {
		t.Errorf("deleted file still on the Mac (err %v)", err)
	}
	trashed, _ := filepath.Glob(filepath.Join(mac.engine.stateDirectory, "trash", "*", filepath.FromSlash(notesPath)))
	if len(trashed) != 1 {
		t.Errorf("trash holds %v", trashed)
	}

	for key := range store.objects {
		delete(store.objects, key)
	}
	mac.pull(t, runOptions{})
	if !strings.Contains(mac.output.String(), "emptied or unreachable") {
		t.Errorf("an empty bucket must not delete local files:\n%s", mac.output)
	}
	if _, err := os.Stat(mac.path(projectPath)); err != nil {
		t.Errorf("project file removed after an empty-bucket pull: %v", err)
	}
}

func TestLargeLocalDeletionNeedsConfirmation(t *testing.T) {
	store, windows, _ := newTwoDevices(t)
	seedWindowsConversation(t, windows)
	windows.push(t, runOptions{})
	if err := os.RemoveAll(filepath.Join(windows.engine.geminiDirectory, "antigravity")); err != nil {
		t.Fatal(err)
	}
	windows.push(t, runOptions{})
	if len(store.objects) != 5 || !strings.Contains(windows.output.String(), "--confirm-deletions") {
		t.Fatalf("wiping the local folder must not wipe the cloud without confirmation (%d objects left)\n%s", len(store.objects), windows.output)
	}
	windows.push(t, runOptions{isDeletionConfirmed: true})
	if len(store.objects) != 1 {
		t.Errorf("confirmed deletion left %d objects, want only the project file", len(store.objects))
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	store, windows, mac := newTwoDevices(t)
	seedWindowsConversation(t, windows)
	windows.push(t, runOptions{isDryRun: true})
	if len(store.objects) != 0 {
		t.Fatalf("dry-run push uploaded %d objects", len(store.objects))
	}
	windows.push(t, runOptions{})
	mac.pull(t, runOptions{isDryRun: true})
	if _, err := os.Stat(mac.path(notesPath)); !os.IsNotExist(err) {
		t.Error("dry-run pull wrote a file")
	}
	if !strings.Contains(mac.output.String(), "would download: 5 file(s)") {
		t.Errorf("dry-run pull output:\n%s", mac.output)
	}
}
