package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// remotePrefix keeps Antigravity's objects in claude-sync's bucket under _external/, which claude-sync's own
// pull and diff skip (it is reserved for data synced by other means), so neither tool sees the other's files.
const remotePrefix = "_external/antigravity/"

const (
	transferWorkers = 8
	listLimit       = 25
	// A run that would delete more than this share of the synced files stops and asks for --confirm-deletions:
	// a half-emptied bucket or folder looks exactly like a large deliberate delete.
	deletionConfirmPercent = 25
)

type syncEngine struct {
	geminiDirectory string
	stateDirectory  string
	store           storage.Storage
	encryptor       *crypto.Encryptor
	paths           *pathConverter
	state           *syncState
	out             io.Writer
	now             func() time.Time
	stateLock       sync.Mutex
}

type runOptions struct {
	isDryRun            bool
	isDeletionConfirmed bool
}

// runSummary counts what a pull or push did. failures are files that hit an error; the run carries on with
// the others and the caller exits non-zero.
type runSummary struct {
	failures int
}

func remoteKey(relativePath string) string { return remotePrefix + relativePath + ".age" }

func (engine *syncEngine) listRemote(ctx context.Context) (map[string]storage.ObjectInfo, error) {
	objects, err := engine.store.List(ctx, remotePrefix)
	if err != nil {
		return nil, fmt.Errorf("list cloud files: %w", err)
	}
	remote := map[string]storage.ObjectInfo{}
	for _, object := range objects {
		if !strings.HasPrefix(object.Key, remotePrefix) || !strings.HasSuffix(object.Key, ".age") {
			continue
		}
		relativePath := strings.TrimSuffix(strings.TrimPrefix(object.Key, remotePrefix), ".age")
		object.ETag = strings.Trim(object.ETag, `"`)
		remote[relativePath] = object
	}
	return remote, nil
}

// currentLocalHash returns the content hash of a local file, reusing the recorded hash when its size and
// modification time are unchanged. A conversation database is always rehashed: Antigravity writes new steps
// to its WAL file first, which leaves the database file's own size and time untouched.
func (engine *syncEngine) currentLocalHash(relativePath string, file localFile) (string, error) {
	if record, isRecorded := engine.state.Files[relativePath]; isRecorded && !isConversationDatabase(relativePath) &&
		record.LocalHash != "" && record.LocalSize == file.size && record.LocalModifiedAt == file.modifiedAt {
		return record.LocalHash, nil
	}
	return localContentHash(relativePath, file.absolutePath)
}

func sortedUnion(maps ...func(func(string))) []string {
	seen := map[string]bool{}
	for _, each := range maps {
		each(func(key string) { seen[key] = true })
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func keysOf[V any](values map[string]V) func(func(string)) {
	return func(visit func(string)) {
		for key := range values {
			visit(key)
		}
	}
}

// ---- pull ----

type pullPlan struct {
	downloads          []string // changed in the cloud, unchanged here (or missing here)
	adoptions          []string // present on both sides and never synced: download and compare
	conflicts          []string // changed on both sides: keep this device's copy, save the cloud's beside it
	keptAnnotations    []string // a title/last-viewed file changed on both sides: keep this device's
	trashes            []string // deleted in the cloud, unchanged here: move to the trash
	keptRemoteDeleted  []string // deleted in the cloud, changed here: kept, and the next push uploads it again
	forgotten          []string // gone on both sides
	isDeletionDeferred bool
}

func (engine *syncEngine) planPull(remote map[string]storage.ObjectInfo, local map[string]localFile) (pullPlan, error) {
	plan := pullPlan{}
	for _, relativePath := range sortedUnion(keysOf(remote), keysOf(local), keysOf(engine.state.Files)) {
		remoteObject, isInCloud := remote[relativePath]
		record, isRecorded := engine.state.Files[relativePath]
		file, isHere := local[relativePath]
		isChangedInCloud := isInCloud && (!isRecorded || remoteObject.ETag != record.RemoteETag)
		switch {
		case isChangedInCloud && !isHere:
			plan.downloads = append(plan.downloads, relativePath)
		case isChangedInCloud && !isRecorded:
			plan.adoptions = append(plan.adoptions, relativePath)
		case isChangedInCloud:
			hash, err := engine.currentLocalHash(relativePath, file)
			if err != nil {
				return plan, err
			}
			switch {
			case hash == record.LocalHash:
				plan.downloads = append(plan.downloads, relativePath)
			case strings.HasPrefix(relativePath, "antigravity/annotations/"):
				plan.keptAnnotations = append(plan.keptAnnotations, relativePath)
			default:
				plan.conflicts = append(plan.conflicts, relativePath)
			}
		case !isInCloud && isRecorded && !isHere:
			plan.forgotten = append(plan.forgotten, relativePath)
		case !isInCloud && isRecorded:
			hash, err := engine.currentLocalHash(relativePath, file)
			if err != nil {
				return plan, err
			}
			if hash == record.LocalHash {
				plan.trashes = append(plan.trashes, relativePath)
			} else {
				plan.keptRemoteDeleted = append(plan.keptRemoteDeleted, relativePath)
			}
		}
	}
	return plan, nil
}

func (engine *syncEngine) pull(ctx context.Context, options runOptions) (runSummary, error) {
	summary := runSummary{}
	remote, err := engine.listRemote(ctx)
	if err != nil {
		return summary, err
	}
	local, err := listLocalFiles(engine.geminiDirectory)
	if err != nil {
		return summary, err
	}
	plan, err := engine.planPull(remote, local)
	if err != nil {
		return summary, err
	}
	switch {
	case len(plan.trashes) > 0 && len(remote) == 0:
		engine.printf("antigravity-sync: the cloud holds no Antigravity files, which looks like an emptied or unreachable bucket; %d deletion(s) not applied\n", len(plan.trashes))
		plan.trashes, plan.isDeletionDeferred = nil, true
	case len(plan.trashes)*100 > len(engine.state.Files)*deletionConfirmPercent && !options.isDeletionConfirmed:
		engine.printf("antigravity-sync: %d of %d synced file(s) were deleted in the cloud, more than %d%%; not applied. Check them, then run pull --confirm-deletions:\n",
			len(plan.trashes), len(engine.state.Files), deletionConfirmPercent)
		engine.printList(plan.trashes)
		plan.trashes, plan.isDeletionDeferred = nil, true
	}
	if options.isDryRun {
		engine.printPullPlan(plan, "would ")
		return summary, nil
	}

	runStamp := engine.now().Format("20060102-150405")
	conflictDirectory := filepath.Join(engine.stateDirectory, "conflicts", runStamp)
	trashDirectory := filepath.Join(engine.stateDirectory, "trash", runStamp)
	var adoptedAsConflict []string
	var listLock sync.Mutex

	failures := runConcurrently(plan.downloads, func(relativePath string) error {
		hash, unknownTokens, err := engine.downloadTo(ctx, relativePath, engine.localPath(relativePath))
		if err != nil {
			return err
		}
		engine.warnUnknownTokens(relativePath, unknownTokens)
		engine.record(relativePath, hash, remote[relativePath].ETag)
		return nil
	})
	failures += runConcurrently(plan.adoptions, func(relativePath string) error {
		temporaryDirectory, err := os.MkdirTemp(engine.stateDirectory, "adopt-")
		if err != nil {
			return err
		}
		defer removeAllOrWarn(temporaryDirectory)
		candidatePath := filepath.Join(temporaryDirectory, filepath.Base(relativePath))
		remoteHash, unknownTokens, err := engine.downloadTo(ctx, relativePath, candidatePath)
		if err != nil {
			return err
		}
		engine.warnUnknownTokens(relativePath, unknownTokens)
		localHash, err := localContentHash(relativePath, engine.localPath(relativePath))
		if err != nil {
			return err
		}
		if localHash == remoteHash {
			engine.record(relativePath, localHash, remote[relativePath].ETag)
			return nil
		}
		if err := moveFile(candidatePath, filepath.Join(conflictDirectory, filepath.FromSlash(relativePath))); err != nil {
			return err
		}
		engine.recordRemoteOnly(relativePath, remote[relativePath].ETag)
		listLock.Lock()
		adoptedAsConflict = append(adoptedAsConflict, relativePath)
		listLock.Unlock()
		return nil
	})
	failures += runConcurrently(plan.conflicts, func(relativePath string) error {
		if _, _, err := engine.downloadTo(ctx, relativePath, filepath.Join(conflictDirectory, filepath.FromSlash(relativePath))); err != nil {
			return err
		}
		engine.recordRemoteOnly(relativePath, remote[relativePath].ETag)
		return nil
	})
	for _, relativePath := range plan.keptAnnotations {
		engine.recordRemoteOnly(relativePath, remote[relativePath].ETag)
	}
	for _, relativePath := range plan.trashes {
		if err := engine.moveToTrash(relativePath, trashDirectory); err != nil {
			warnf("move %s to the trash: %v", relativePath, err)
			failures++
			continue
		}
		delete(engine.state.Files, relativePath)
	}
	for _, relativePath := range plan.forgotten {
		delete(engine.state.Files, relativePath)
	}
	if err := engine.state.save(); err != nil {
		return summary, fmt.Errorf("save sync state: %w", err)
	}
	plan.conflicts = append(plan.conflicts, adoptedAsConflict...)
	engine.printPullPlan(plan, "")
	if len(plan.conflicts) > 0 {
		engine.printf("antigravity-sync: the cloud's copies of those conflicts are in %s; the next push uploads this device's\n", conflictDirectory)
	}
	if len(plan.trashes) > 0 {
		engine.printf("antigravity-sync: moved files are in %s\n", trashDirectory)
	}
	summary.failures = failures
	return summary, nil
}

func (engine *syncEngine) printPullPlan(plan pullPlan, verbPrefix string) {
	engine.printCategory(verbPrefix+"download", plan.downloads)
	engine.printCategory(verbPrefix+"compare (present on both sides, never synced)", plan.adoptions)
	engine.printCategory(verbPrefix+"keep this device's copy of conflicts (changed on both sides)", plan.conflicts)
	engine.printCategory(verbPrefix+"keep this device's title files (changed on both sides)", plan.keptAnnotations)
	engine.printCategory(verbPrefix+"move to the trash (deleted in the cloud)", plan.trashes)
	engine.printCategory("keep, deleted in the cloud but changed here (the next push uploads them)", plan.keptRemoteDeleted)
	if len(plan.downloads)+len(plan.adoptions)+len(plan.conflicts)+len(plan.keptAnnotations)+len(plan.trashes)+len(plan.keptRemoteDeleted) == 0 && !plan.isDeletionDeferred {
		engine.printf("antigravity-sync: nothing to pull\n")
	}
}

// ---- push ----

type pushPlan struct {
	uploads             []string
	localHashes         map[string]string
	deletions           []string
	skippedCloudChanged []string // changed in the cloud since this device last synced: pull first
	forgotten           []string
	isDeletionDeferred  bool
}

func (engine *syncEngine) planPush(remote map[string]storage.ObjectInfo, local map[string]localFile) (pushPlan, error) {
	plan := pushPlan{localHashes: map[string]string{}}
	for _, relativePath := range sortedUnion(keysOf(remote), keysOf(local), keysOf(engine.state.Files)) {
		remoteObject, isInCloud := remote[relativePath]
		record, isRecorded := engine.state.Files[relativePath]
		file, isHere := local[relativePath]
		isChangedInCloud := isInCloud && (!isRecorded || remoteObject.ETag != record.RemoteETag)
		switch {
		case isHere:
			hash, err := engine.currentLocalHash(relativePath, file)
			if err != nil {
				return plan, err
			}
			if isRecorded && hash == record.LocalHash {
				continue
			}
			if isChangedInCloud {
				plan.skippedCloudChanged = append(plan.skippedCloudChanged, relativePath)
				continue
			}
			plan.uploads = append(plan.uploads, relativePath)
			plan.localHashes[relativePath] = hash
		case isRecorded && !isInCloud:
			plan.forgotten = append(plan.forgotten, relativePath)
		case isRecorded && isChangedInCloud:
			plan.skippedCloudChanged = append(plan.skippedCloudChanged, relativePath)
		case isRecorded:
			plan.deletions = append(plan.deletions, relativePath)
		}
	}
	return plan, nil
}

func (engine *syncEngine) push(ctx context.Context, options runOptions) (runSummary, error) {
	summary := runSummary{}
	remote, err := engine.listRemote(ctx)
	if err != nil {
		return summary, err
	}
	local, err := listLocalFiles(engine.geminiDirectory)
	if err != nil {
		return summary, err
	}
	plan, err := engine.planPush(remote, local)
	if err != nil {
		return summary, err
	}
	if len(plan.deletions)*100 > len(engine.state.Files)*deletionConfirmPercent && !options.isDeletionConfirmed {
		engine.printf("antigravity-sync: %d of %d synced file(s) are gone from this device, more than %d%%; not deleted from the cloud. Check them, then run push --confirm-deletions:\n",
			len(plan.deletions), len(engine.state.Files), deletionConfirmPercent)
		engine.printList(plan.deletions)
		plan.deletions, plan.isDeletionDeferred = nil, true
	}
	if options.isDryRun {
		engine.printPushPlan(plan, "would ")
		return summary, nil
	}

	uploadedHashes := map[string]string{}
	var hashLock sync.Mutex
	failures := runConcurrently(plan.uploads, func(relativePath string) error {
		portable, hash, err := engine.preparePortable(relativePath, local[relativePath])
		if err != nil {
			return err
		}
		if err := engine.upload(ctx, relativePath, portable); err != nil {
			return err
		}
		hashLock.Lock()
		uploadedHashes[relativePath] = hash
		hashLock.Unlock()
		return nil
	})
	if len(plan.deletions) > 0 {
		keys := make([]string, len(plan.deletions))
		for i, relativePath := range plan.deletions {
			keys[i] = remoteKey(relativePath)
		}
		if err := engine.store.DeleteBatch(ctx, keys); err != nil {
			warnf("delete %d file(s) from the cloud: %v", len(keys), err)
			failures += len(keys)
		} else {
			for _, relativePath := range plan.deletions {
				delete(engine.state.Files, relativePath)
			}
		}
	}
	for _, relativePath := range plan.forgotten {
		delete(engine.state.Files, relativePath)
	}
	if len(uploadedHashes) > 0 {
		// The cloud assigns each upload a new ETag; read them back so the next run knows these copies.
		uploadedRemote, err := engine.listRemote(ctx)
		if err != nil {
			warnf("read back uploaded files: %v; the next pull compares them with this device again", err)
		}
		for relativePath, hash := range uploadedHashes {
			if object, isListed := uploadedRemote[relativePath]; isListed {
				engine.recordWithFile(relativePath, hash, object.ETag, local[relativePath])
			}
		}
	}
	if err := engine.state.save(); err != nil {
		return summary, fmt.Errorf("save sync state: %w", err)
	}
	engine.printPushPlan(plan, "")
	summary.failures = failures
	return summary, nil
}

func (engine *syncEngine) printPushPlan(plan pushPlan, verbPrefix string) {
	engine.printCategory(verbPrefix+"upload", plan.uploads)
	engine.printCategory(verbPrefix+"delete from the cloud (deleted on this device)", plan.deletions)
	engine.printCategory("skip, changed in the cloud since the last sync (pull first)", plan.skippedCloudChanged)
	if len(plan.uploads)+len(plan.deletions)+len(plan.skippedCloudChanged) == 0 && !plan.isDeletionDeferred {
		engine.printf("antigravity-sync: nothing to push\n")
	}
}

// ---- content in each direction ----

// preparePortable reads a local file and returns its content with this device's folders replaced by portable
// tokens, and the hash of the local content it was made from.
func (engine *syncEngine) preparePortable(relativePath string, file localFile) ([]byte, string, error) {
	if isConversationDatabase(relativePath) {
		temporaryDirectory, err := os.MkdirTemp(engine.stateDirectory, "snapshot-")
		if err != nil {
			return nil, "", err
		}
		defer removeAllOrWarn(temporaryDirectory)
		snapshotPath := filepath.Join(temporaryDirectory, "conversation.db")
		if err := snapshotDatabase(file.absolutePath, snapshotPath); err != nil {
			return nil, "", err
		}
		hash, err := databaseContentHash(snapshotPath)
		if err != nil {
			return nil, "", err
		}
		report, err := convertDatabaseFile(snapshotPath, engine.toPortableText, engine.paths.containsLocalRoot)
		if err != nil {
			return nil, "", err
		}
		if report.unhandledFields > 0 {
			warnf("%s: %d binary field(s) name a mapped folder and were left unconverted", relativePath, report.unhandledFields)
		}
		portable, err := os.ReadFile(snapshotPath)
		return portable, hash, err
	}
	data, err := os.ReadFile(file.absolutePath)
	if err != nil {
		return nil, "", err
	}
	hash := bytesHash(data)
	if isCleanText(data) {
		data = engine.toPortableText(data)
	}
	return data, hash, nil
}

func (engine *syncEngine) toPortableText(data []byte) []byte {
	converted, _ := engine.paths.toPortable(data)
	return converted
}

// materialize writes cloud content to destinationPath in this device's form, through a temporary file in the
// same folder that is renamed into place. It returns the hash of the written content and the number of
// tokens this device has no folder for. For a conversation database it also removes the WAL and shared-memory
// files left beside the file it replaces, which belonged to that file.
func (engine *syncEngine) materialize(relativePath string, portable []byte, destinationPath string) (string, int, error) {
	directory := filepath.Dir(destinationPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", 0, err
	}
	temporary, err := os.CreateTemp(directory, temporaryFilePrefix+"*.tmp")
	if err != nil {
		return "", 0, err
	}
	temporaryPath := temporary.Name()
	isRenamed := false
	defer func() {
		if !isRenamed {
			removeOrWarn(temporaryPath)
		}
	}()
	unknownTokens := 0
	content := portable
	if !isConversationDatabase(relativePath) && isCleanText(portable) {
		content, unknownTokens = engine.paths.toLocal(portable)
	}
	if _, err := temporary.Write(content); err != nil {
		closeFileOrWarn(temporary)
		return "", 0, err
	}
	if err := temporary.Close(); err != nil {
		return "", 0, err
	}
	hash := bytesHash(content)
	if isConversationDatabase(relativePath) {
		convertText := func(data []byte) []byte {
			converted, unknown := engine.paths.toLocal(data)
			unknownTokens += unknown
			return converted
		}
		if _, err := convertDatabaseFile(temporaryPath, convertText, containsPortableToken); err != nil {
			return "", 0, err
		}
		if hash, err = databaseContentHash(temporaryPath); err != nil {
			return "", 0, err
		}
		for _, sideFile := range []string{destinationPath + "-wal", destinationPath + "-shm"} {
			if err := os.Remove(sideFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", 0, fmt.Errorf("remove %s: %w", sideFile, err)
			}
		}
	}
	if err := os.Rename(temporaryPath, destinationPath); err != nil {
		return "", 0, err
	}
	isRenamed = true
	return hash, unknownTokens, nil
}

func (engine *syncEngine) downloadTo(ctx context.Context, relativePath, destinationPath string) (string, int, error) {
	encrypted, err := engine.store.Download(ctx, remoteKey(relativePath))
	if err != nil {
		return "", 0, fmt.Errorf("download: %w", err)
	}
	compressed, err := engine.encryptor.Decrypt(encrypted)
	if err != nil {
		return "", 0, fmt.Errorf("decrypt: %w", err)
	}
	portable, err := gzipDecompress(compressed)
	if err != nil {
		return "", 0, fmt.Errorf("decompress: %w", err)
	}
	return engine.materialize(relativePath, portable, destinationPath)
}

func (engine *syncEngine) upload(ctx context.Context, relativePath string, portable []byte) error {
	compressed, err := gzipCompress(portable)
	if err != nil {
		return fmt.Errorf("compress: %w", err)
	}
	encrypted, err := engine.encryptor.Encrypt(compressed)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	if err := engine.store.Upload(ctx, remoteKey(relativePath), encrypted); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

// ---- state ----

func (engine *syncEngine) localPath(relativePath string) string {
	return filepath.Join(engine.geminiDirectory, filepath.FromSlash(relativePath))
}

// record notes that relativePath is in step with the cloud copy remoteETag, reading the written file's size
// and time so an unchanged file is not rehashed next time.
func (engine *syncEngine) record(relativePath, localHash, remoteETag string) {
	file := localFile{}
	if info, err := os.Stat(engine.localPath(relativePath)); err == nil {
		file = localFile{size: info.Size(), modifiedAt: info.ModTime().UnixNano()}
	} else {
		warnf("stat %s after writing it: %v; it is rehashed next time", relativePath, err)
	}
	engine.recordWithFile(relativePath, localHash, remoteETag, file)
}

func (engine *syncEngine) recordWithFile(relativePath, localHash, remoteETag string, file localFile) {
	engine.stateLock.Lock()
	defer engine.stateLock.Unlock()
	engine.state.Files[relativePath] = syncedFileRecord{
		LocalHash: localHash, LocalSize: file.size, LocalModifiedAt: file.modifiedAt,
		RemoteETag: remoteETag, SyncedAt: engine.now(),
	}
}

// recordRemoteOnly notes that the cloud copy remoteETag has been seen while this device keeps its own,
// changed copy: the local hash stays as last synced, so the next push uploads this device's copy.
func (engine *syncEngine) recordRemoteOnly(relativePath, remoteETag string) {
	engine.stateLock.Lock()
	defer engine.stateLock.Unlock()
	record := engine.state.Files[relativePath]
	record.RemoteETag = remoteETag
	record.LocalSize, record.LocalModifiedAt = 0, 0
	engine.state.Files[relativePath] = record
}

func (engine *syncEngine) warnUnknownTokens(relativePath string, unknownTokens int) {
	if unknownTokens > 0 {
		warnf("%s: %d path token(s) have no folder on this device; add the matching path_map entry to claude-sync's config", relativePath, unknownTokens)
	}
}

// ---- helpers ----

// runConcurrently runs work on each item with a few workers and returns how many failed; each failure is
// reported with its item.
func runConcurrently(items []string, work func(string) error) int {
	if len(items) == 0 {
		return 0
	}
	queue := make(chan string)
	var failures int
	var failureLock sync.Mutex
	var workers sync.WaitGroup
	for range min(transferWorkers, len(items)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range queue {
				if err := work(item); err != nil {
					warnf("%s: %v", item, err)
					failureLock.Lock()
					failures++
					failureLock.Unlock()
				}
			}
		}()
	}
	for _, item := range items {
		queue <- item
	}
	close(queue)
	workers.Wait()
	return failures
}

// moveToTrash moves a local file to the same relative path under trashDirectory. A conversation database
// takes its WAL and shared-memory files with it, so no side file is left without its database.
func (engine *syncEngine) moveToTrash(relativePath, trashDirectory string) error {
	sourcePath := engine.localPath(relativePath)
	destinationPath := filepath.Join(trashDirectory, filepath.FromSlash(relativePath))
	if err := moveFile(sourcePath, destinationPath); err != nil {
		return err
	}
	if isConversationDatabase(relativePath) {
		for _, suffix := range []string{"-wal", "-shm"} {
			if err := moveFile(sourcePath+suffix, destinationPath+suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func moveFile(sourcePath, destinationPath string) error {
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o700); err != nil {
		return err
	}
	return os.Rename(sourcePath, destinationPath)
}

func removeOrWarn(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		warnf("remove %s: %v", path, err)
	}
}

func removeAllOrWarn(path string) {
	if err := os.RemoveAll(path); err != nil {
		warnf("remove %s: %v", path, err)
	}
}

func gzipCompress(data []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func gzipDecompress(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := reader.Close(); err != nil {
			warnf("close gzip reader: %v", err)
		}
	}()
	return io.ReadAll(reader)
}

func (engine *syncEngine) printf(format string, arguments ...any) {
	fmt.Fprintf(engine.out, format, arguments...)
}

func (engine *syncEngine) printCategory(label string, paths []string) {
	if len(paths) == 0 {
		return
	}
	engine.printf("antigravity-sync: %s: %d file(s)\n", label, len(paths))
	engine.printList(paths)
}

func (engine *syncEngine) printList(paths []string) {
	for i, path := range paths {
		if i == listLimit {
			engine.printf("    ... and %d more\n", len(paths)-listLimit)
			return
		}
		engine.printf("    %s\n", path)
	}
}

func warnf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "antigravity-sync: warning: "+format+"\n", arguments...)
}
