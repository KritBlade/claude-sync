package sync

import (
	"encoding/json"
	"testing"
)

// Cross-OS content mapping. A Windows device and a POSIX device spell the same
// project path differently, and .jsonl escapes a backslash as a pair, so the
// normalized remote form has to be separator- and escaping-neutral.

const (
	posixHome = `/Users/alice`
	posixRoot = `/Users/alice/projects`
	winHome   = `C:\Users\bob`
	winRoot   = `D:\projects`
	winRoot2  = `C:\work\projects`

	// Underscore and dash both belong to a path segment, so a name using them
	// exercises the boundary rules.
	project = `my_app-dev`

	sessionFile = "projects/whatever/s.jsonl"
	memoFile    = "projects/whatever/memory/note.md"
)

func mapperFor(t *testing.T, home, root string) *PathMapper {
	t.Helper()
	m, err := NewPathMapper(home, map[string]string{root: "WORK"})
	if err != nil {
		t.Fatalf("NewPathMapper(%q, %q): %v", home, root, err)
	}
	return m
}

func TestContentNormalizesToSameRemoteFormOnEveryOS(t *testing.T) {
	posix := mapperFor(t, posixHome, posixRoot)
	win := mapperFor(t, winHome, winRoot)

	want := `{"cwd":"${WORK}/my_app-dev"}`

	got := string(posix.NormalizeContent(sessionFile, []byte(`{"cwd":"/Users/alice/projects/my_app-dev"}`)))
	if got != want {
		t.Errorf("posix normalize = %s, want %s", got, want)
	}

	got = string(win.NormalizeContent(sessionFile, []byte(`{"cwd":"D:\\projects\\my_app-dev"}`)))
	if got != want {
		t.Errorf("windows normalize = %s, want %s", got, want)
	}

	// A Windows root spelled with forward slashes is deliberately left alone:
	// that form appears mostly in quoted error text and tool output, where
	// rewriting the device's own prose would be worse than leaving it unmapped.
	prose := `{"text":"failed at D:/projects/my_app-dev"}`
	if got := string(win.NormalizeContent(sessionFile, []byte(prose))); got != prose {
		t.Errorf("forward-slash root = %s, should be untouched", got)
	}
}

func TestContentRoundTripAcrossOS(t *testing.T) {
	posix := mapperFor(t, posixHome, posixRoot)
	win := mapperFor(t, winHome, winRoot2)

	cases := []struct {
		name     string
		from, to *PathMapper
		relPath  string
		in, want string
	}{
		{
			name:    "posix to windows, jsonl stays valid JSON",
			from:    posix,
			to:      win,
			relPath: sessionFile,
			in:      `{"cwd":"/Users/alice/projects/my_app-dev","home":"/Users/alice/.claude"}`,
			want:    `{"cwd":"C:\\work\\projects\\my_app-dev","home":"C:\\Users\\bob\\.claude"}`,
		},
		{
			name:    "windows to posix, jsonl",
			from:    win,
			to:      posix,
			relPath: sessionFile,
			in:      `{"cwd":"C:\\work\\projects\\my_app-dev","home":"C:\\Users\\bob\\.claude"}`,
			want:    `{"cwd":"/Users/alice/projects/my_app-dev","home":"/Users/alice/.claude"}`,
		},
		{
			name:    "windows to posix, markdown keeps raw separators",
			from:    win,
			to:      posix,
			relPath: memoFile,
			in:      `see C:\work\projects\my_app-dev for details`,
			want:    `see /Users/alice/projects/my_app-dev for details`,
		},
		{
			name:    "posix to windows, markdown",
			from:    posix,
			to:      win,
			relPath: memoFile,
			in:      `see /Users/alice/projects/my_app-dev for details`,
			want:    `see C:\work\projects\my_app-dev for details`,
		},
		{
			name:    "windows to windows is unchanged end to end",
			from:    win,
			to:      win,
			relPath: sessionFile,
			in:      `{"cwd":"C:\\work\\projects\\my_app-dev"}`,
			want:    `{"cwd":"C:\\work\\projects\\my_app-dev"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			norm := tc.from.NormalizeContent(tc.relPath, []byte(tc.in))
			got := string(tc.to.ResolveContent(tc.relPath, norm))
			if got != tc.want {
				t.Errorf("round trip = %s\nwant                = %s\n(remote form was %s)", got, tc.want, norm)
			}
		})
	}
}

func TestResolvedJSONStaysParseable(t *testing.T) {
	posix := mapperFor(t, posixHome, posixRoot)
	win := mapperFor(t, winHome, winRoot)

	in := []byte(`{"cwd":"/Users/alice/projects/my_app-dev","home":"/Users/alice/.claude"}`)
	out := win.ResolveContent(sessionFile, posix.NormalizeContent(sessionFile, in))

	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("resolved content is not valid JSON: %v\ncontent: %s", err, out)
	}
	if v["cwd"] != `D:\projects\my_app-dev` {
		t.Errorf("decoded cwd = %v", v["cwd"])
	}
}

func TestWindowsBoundariesNotOverMatched(t *testing.T) {
	win := mapperFor(t, winHome, winRoot)

	// A longer sibling directory must not match the mapped root.
	for _, s := range []string{
		`{"cwd":"D:\\projectsOther\\x"}`,
		`{"cwd":"D:\\projects-archive\\x"}`,
	} {
		if got := string(win.NormalizeContent(sessionFile, []byte(s))); got != s {
			t.Errorf("NormalizeContent(%s) = %s, should be untouched", s, got)
		}
	}
}

func TestRemoteKeysMapAcrossOS(t *testing.T) {
	posix := mapperFor(t, posixHome, posixRoot)
	win := mapperFor(t, winHome, winRoot2)

	local := "projects/" + EncodeClaudePath(posixRoot+"/"+project) + "/s.jsonl"
	remote := posix.NormalizeRelPath(local)
	got, ok := win.ResolveRelPath(remote)

	want := "projects/" + EncodeClaudePath(winRoot2+`\`+project) + "/s.jsonl"
	if !ok || got != want {
		t.Errorf("ResolveRelPath(%q) = %q (ok=%v), want %q", remote, got, ok, want)
	}
}
