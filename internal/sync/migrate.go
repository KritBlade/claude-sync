package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MigrateResult describes the outcome of migrating legacy remote keys to
// portable (path-normalized) keys.
type MigrateResult struct {
	Migrated []string // local paths re-uploaded under normalized keys
	Foreign  []string // legacy keys owned by another device (run migrate there)
	Errors   []error
}

// MigratePaths rewrites this device's legacy remote project keys to the
// portable token form: each file is re-uploaded under its normalized key
// (with content normalization applied) and the legacy key is deleted.
//
// Keys that don't match any of this device's path mappings — typically
// projects pushed from another machine — are left untouched and reported in
// Foreign; running migrate on that machine completes the migration.
func (s *Syncer) MigratePaths(ctx context.Context) (*MigrateResult, error) {
	result := &MigrateResult{}

	remoteObjects, err := s.storage.List(ctx, "projects/")
	if err != nil {
		return nil, fmt.Errorf("failed to list remote objects: %w", err)
	}

	// A legacy remote key and the local path it is read from differ whenever the
	// key was written with an uppercase drive letter.
	type legacyKey struct {
		remoteKey string
		localPath string
	}
	var legacyKeys []legacyKey
	for _, obj := range remoteObjects {
		if !strings.HasSuffix(obj.Key, ".age") {
			continue
		}
		raw := strings.TrimSuffix(obj.Key, ".age")
		if seg, _, ok := splitProjectsPath(raw); !ok || strings.HasPrefix(seg, tokenPrefix) {
			continue // already normalized
		}
		if s.isExcluded(raw) {
			continue
		}
		normalized := s.paths.NormalizeRelPath(raw)
		if normalized == raw {
			// Not under any of this device's mapped prefixes
			result.Foreign = append(result.Foreign, raw)
			continue
		}
		// A legacy key carries whatever drive-letter case the folder had when it
		// was written. Read and record the file under the canonical spelling, or
		// the upload puts the old one straight back into the state this device
		// just rewrote.
		local := canonicalRelPath(raw)
		if _, err := os.Stat(filepath.Join(s.claudeDir, local)); err != nil {
			// We can't re-encrypt content we don't have locally
			result.Foreign = append(result.Foreign, raw)
			continue
		}
		legacyKeys = append(legacyKeys, legacyKey{remoteKey: raw, localPath: local})
	}

	total := len(legacyKeys)
	for i, key := range legacyKeys {
		s.progress(ProgressEvent{Action: "upload", Path: key.localPath, Current: i + 1, Total: total})
		if err := s.uploadFile(ctx, key.localPath); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s: %w", key.localPath, err))
			continue
		}
		if err := s.storage.Delete(ctx, key.remoteKey+".age"); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("delete legacy %s: %w", key.remoteKey, err))
			continue
		}
		result.Migrated = append(result.Migrated, key.remoteKey)
	}

	if total > 0 {
		if err := s.state.Save(); err != nil {
			return result, fmt.Errorf("failed to save state: %w", err)
		}
	}

	return result, nil
}
