package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeJunction links link to the directory target with a directory junction,
// the kind of link a skill installer creates on Windows. mklink /J needs no
// administrator rights, unlike a symlink.
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create a junction here: %v: %s", err, output)
	}
}

func writeSkillFile(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# skill"), 0600); err != nil {
		t.Fatalf("write SKILL.md in %s: %v", dir, err)
	}
}

func TestGetLocalFilesSkipsJunctionsInDirectories(t *testing.T) {
	claudeDir := t.TempDir()
	elsewhere := t.TempDir()

	writeSkillFile(t, filepath.Join(claudeDir, "skills", "own-skill"))
	writeSkillFile(t, filepath.Join(elsewhere, "linked-skill"))
	makeJunction(t, filepath.Join(claudeDir, "skills", "linked-skill"), filepath.Join(elsewhere, "linked-skill"))

	localFiles, err := GetLocalFiles(claudeDir, []string{"skills"})
	if err != nil {
		t.Fatalf("GetLocalFiles failed on a folder holding a junction: %v", err)
	}

	if _, ok := localFiles["skills/own-skill/SKILL.md"]; !ok {
		t.Error("regular file beside the junction should be listed")
	}
	for relPath := range localFiles {
		if relPath == "skills/linked-skill" || filepath.Dir(filepath.FromSlash(relPath)) == filepath.FromSlash("skills/linked-skill") {
			t.Errorf("junction or its content should not be listed, got %q", relPath)
		}
	}

	// Every listed path must read as a file, since push uploads each one.
	for relPath := range localFiles {
		if _, err := os.ReadFile(filepath.Join(claudeDir, filepath.FromSlash(relPath))); err != nil {
			t.Errorf("listed path %q does not read as a file: %v", relPath, err)
		}
	}
}

func TestGetLocalFilesSkipsATopLevelJunction(t *testing.T) {
	claudeDir := t.TempDir()
	elsewhere := t.TempDir()

	writeSkillFile(t, filepath.Join(elsewhere, "skills", "linked-skill"))
	makeJunction(t, filepath.Join(claudeDir, "skills"), filepath.Join(elsewhere, "skills"))

	localFiles, err := GetLocalFiles(claudeDir, []string{"skills"})
	if err != nil {
		t.Fatalf("GetLocalFiles failed on a sync path that is a junction: %v", err)
	}
	if len(localFiles) != 0 {
		t.Errorf("a sync path that is a junction should list nothing, got %v", localFiles)
	}
}
