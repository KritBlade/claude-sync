// antigravity-sync syncs Antigravity conversations (~/.gemini/antigravity) between devices through
// claude-sync's encrypted bucket, converting the folder paths inside them between Windows and macOS.
//
//	antigravity-sync pull   [--dry-run] [--confirm-deletions] [--stop-antigravity]
//	antigravity-sync push   [--dry-run] [--confirm-deletions]
//	antigravity-sync status
//	antigravity-sync version
//
// It reads claude-sync's config (~/.claude-sync/config.yaml) for the bucket, the encryption key and path_map,
// stores its files under _external/antigravity/ in that bucket, and keeps its own state in
// ~/.claude-sync/antigravity/.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/storage"
	_ "github.com/tawanorg/claude-sync/internal/storage/gcs"
	_ "github.com/tawanorg/claude-sync/internal/storage/r2"
	_ "github.com/tawanorg/claude-sync/internal/storage/s3"
	_ "github.com/tawanorg/claude-sync/internal/storage/webdav"
)

var version = "dev"

const usageText = `usage:
  antigravity-sync pull   [--dry-run] [--confirm-deletions] [--stop-antigravity]
  antigravity-sync push   [--dry-run] [--confirm-deletions]
  antigravity-sync status   (what pull and push would do)
  antigravity-sync version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, out, errorOut io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprint(errorOut, usageText)
		return 2
	}
	command := arguments[0]
	switch command {
	case "version", "--version", "-v":
		fmt.Fprintf(out, "antigravity-sync %s\n", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(out, usageText)
		return 0
	case "pull", "push", "status":
	default:
		fmt.Fprintf(errorOut, "antigravity-sync: unknown command %q\n%s", command, usageText)
		return 2
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(errorOut)
	isDryRun := flags.Bool("dry-run", false, "show what would change, and change nothing")
	isDeletionConfirmed := flags.Bool("confirm-deletions", false, "apply deletions even when they exceed the safety share")
	shouldStopAntigravity := flags.Bool("stop-antigravity", false, "pull: stop a running agy hub first")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errorOut, "antigravity-sync: unexpected argument %q\n%s", flags.Arg(0), usageText)
		return 2
	}

	engine, err := newEngineFromConfig(out)
	if err != nil {
		fmt.Fprintf(errorOut, "antigravity-sync: %v\n", err)
		return 1
	}
	ctx := context.Background()
	options := runOptions{isDryRun: *isDryRun, isDeletionConfirmed: *isDeletionConfirmed}
	var summary runSummary
	switch command {
	case "status":
		options.isDryRun = true
		if _, err = engine.pull(ctx, options); err == nil {
			_, err = engine.push(ctx, options)
		}
	case "pull":
		if !options.isDryRun {
			if err := ensureAntigravityStopped(*shouldStopAntigravity); err != nil {
				fmt.Fprintf(errorOut, "antigravity-sync: pull not run: %v\n", err)
				return 1
			}
		}
		summary, err = engine.pull(ctx, options)
	case "push":
		summary, err = engine.push(ctx, options)
	}
	if err != nil {
		fmt.Fprintf(errorOut, "antigravity-sync: %s failed: %v\n", command, err)
		return 1
	}
	if summary.failures > 0 {
		fmt.Fprintf(errorOut, "antigravity-sync: %s finished with %d file(s) failed (see the warnings above)\n", command, summary.failures)
		return 1
	}
	return 0
}

func newEngineFromConfig(out io.Writer) (*syncEngine, error) {
	claudeSyncConfig, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("read claude-sync config: %w", err)
	}
	store, err := storage.New(claudeSyncConfig.GetStorageConfig())
	if err != nil {
		return nil, fmt.Errorf("open cloud storage: %w", err)
	}
	encryptor, err := crypto.NewEncryptor(claudeSyncConfig.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("load encryption key: %w", err)
	}
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths, err := newPathConverter(homeDirectory, claudeSyncConfig.PathMap)
	if err != nil {
		return nil, err
	}
	stateDirectory := filepath.Join(config.ConfigDirPath(), "antigravity")
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return nil, err
	}
	state, err := loadSyncState(filepath.Join(stateDirectory, "state.json"))
	if err != nil {
		return nil, err
	}
	return &syncEngine{
		geminiDirectory: filepath.Join(homeDirectory, ".gemini"),
		stateDirectory:  stateDirectory,
		store:           store,
		encryptor:       encryptor,
		paths:           paths,
		state:           state,
		out:             out,
		now:             time.Now,
	}, nil
}
