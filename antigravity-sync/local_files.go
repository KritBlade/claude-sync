package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// syncedFolders are the parts of ~/.gemini that carry Antigravity conversations, relative to ~/.gemini:
// the conversations themselves, their artifacts and uploads (brain), their titles (annotations), and the
// project definitions that tie a conversation to a workspace folder.
var syncedFolders = []string{
	"antigravity/conversations",
	"antigravity/brain",
	"antigravity/annotations",
	"config/projects",
}

// temporaryFilePrefix names this program's own temporary files, which are never synced.
const temporaryFilePrefix = ".antigravity-sync-"

type localFile struct {
	absolutePath string
	size         int64
	modifiedAt   int64 // Unix nanoseconds
}

// isSyncedPath reports whether a path (relative to ~/.gemini, "/"-separated) is synced. Excluded: SQLite's
// side files, lock files, Finder metadata, this program's temporary files, files in conversations/ that are
// not a conversation, and the default CLI project, which every installation creates for itself.
func isSyncedPath(relativePath string) bool {
	name := path.Base(relativePath)
	switch {
	case name == ".DS_Store",
		strings.HasPrefix(name, temporaryFilePrefix),
		strings.HasSuffix(name, "-wal"), strings.HasSuffix(name, "-shm"), strings.HasSuffix(name, "-journal"),
		strings.HasSuffix(name, ".lock"),
		relativePath == "config/projects/default-cli-project.json":
		return false
	case strings.HasPrefix(relativePath, "antigravity/conversations/"):
		return path.Dir(relativePath) == "antigravity/conversations" &&
			(strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".pb"))
	}
	return true
}

// listLocalFiles returns the synced regular files under geminiDirectory, keyed by "/"-separated path relative
// to it. Symlinks, junctions and other non-regular entries are skipped, as claude-sync skips them.
func listLocalFiles(geminiDirectory string) (map[string]localFile, error) {
	files := map[string]localFile{}
	for _, folder := range syncedFolders {
		root := filepath.Join(geminiDirectory, filepath.FromSlash(folder))
		if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(root, func(absolutePath string, entry fs.DirEntry, walkError error) error {
			if walkError != nil {
				return walkError
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("stat %s: %w", absolutePath, err)
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			relative, err := filepath.Rel(geminiDirectory, absolutePath)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if !isSyncedPath(relative) {
				return nil
			}
			files[relative] = localFile{absolutePath: absolutePath, size: info.Size(), modifiedAt: info.ModTime().UnixNano()}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", root, err)
		}
	}
	return files, nil
}

// localContentHash identifies a file's content: the rows for a conversation database, the bytes otherwise.
func localContentHash(relativePath, absolutePath string) (string, error) {
	if isConversationDatabase(relativePath) {
		return databaseContentHash(absolutePath)
	}
	return fileHash(absolutePath)
}

func fileHash(absolutePath string) (string, error) {
	file, err := os.Open(absolutePath)
	if err != nil {
		return "", err
	}
	defer closeFileOrWarn(file)
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", absolutePath, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func bytesHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func closeFileOrWarn(file *os.File) {
	if err := file.Close(); err != nil {
		warnf("close %s: %v", file.Name(), err)
	}
}
