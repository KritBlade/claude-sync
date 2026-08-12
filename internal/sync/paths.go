package sync

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// PathMapper translates machine-specific project paths to portable tokens on
// push and back to local paths on pull, so sessions started on one device are
// resumable on another even when home directories or project layouts differ.
//
// Claude Code stores sessions under ~/.claude/projects/<encoded-cwd>/ where
// <encoded-cwd> is the working directory with every non-alphanumeric character
// replaced by "-" (e.g. /Users/alice/my-app -> -Users-alice-my-app). Because
// the encoding is keyed to the absolute path, a transcript synced verbatim to
// a machine with a different username or layout lands in a directory that
// `claude --resume` never looks at.
//
// The mapper rewrites two things:
//   - remote keys:   projects/-Users-alice-my-app/... -> projects/${HOME}-my-app/...
//   - file content:  /Users/alice -> ${HOME} (cwd fields, tool paths)
//
// HOME is always mapped. Additional prefixes (e.g. ~/work on one machine,
// ~/Projects on another) can be mapped via the path_map config, with both
// machines pointing their own local path at the same token name.
//
// Content rewriting is separator- and escaping-aware, because a Windows device
// and a POSIX device do not spell the same path the same way. Remotely, path
// tails are always canonicalized to "/" separators; on pull they are rendered
// with the local separator, escaped for the file format being written.
type PathMapper struct {
	// mappings ordered longest local path first so the most specific prefix wins
	mappings []pathMapping
}

// pathContentKind describes how a file format spells a path, which decides both
// what to match on push and what to emit on pull.
type pathContentKind int

const (
	// pathContentPlain holds paths verbatim (.md, .txt).
	pathContentPlain pathContentKind = iota
	// pathContentJSON escapes a backslash as a pair (.json, .jsonl), so a Windows
	// path reads as C:\\Users\\bob. Writing a raw backslash into one of these
	// produces an invalid escape sequence and the file stops parsing.
	pathContentJSON
	numPathContentKinds
)

// pathSegment matches one path component. Deliberately conservative: a tail
// stops at the first character that is not plainly part of a file name, so
// prose following a path is never rewritten.
const pathSegment = `[A-Za-z0-9_.-]*`

type pathMapping struct {
	name      string // token name, e.g. "HOME", "WORK"
	localPath string // absolute local path, no trailing separator
	encLocal  string // localPath in Claude Code's directory encoding
	windows   bool   // localPath uses backslash separators

	// normRe matches this device's spelling of localPath (plus any path tail)
	// for a given content kind; localIn is how localPath is written back out.
	normRe  [numPathContentKinds]*regexp.Regexp
	localIn [numPathContentKinds]string

	// resolveRe matches the portable token and its canonical "/" tail. The
	// token is self-delimiting, so no trailing boundary is needed.
	resolveRe *regexp.Regexp
}

var pathTokenNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// NewPathMapper builds a mapper for this device. userMap maps local absolute
// paths (already ~-expanded) to token names shared across devices.
func NewPathMapper(homeDir string, userMap map[string]string) (*PathMapper, error) {
	m := &PathMapper{}

	add := func(name, localPath string) error {
		localPath = strings.TrimRight(localPath, `/\`)
		if localPath == "" {
			return nil
		}
		if !pathTokenNameRe.MatchString(name) {
			return fmt.Errorf("invalid path_map token %q: use uppercase letters, digits, underscores (e.g. WORK)", name)
		}

		mp := pathMapping{
			name:      name,
			localPath: localPath,
			encLocal:  EncodeClaudePath(localPath),
			windows:   strings.Contains(localPath, `\`),
		}

		for _, kind := range []pathContentKind{pathContentPlain, pathContentJSON} {
			// Only the root's native spelling is matched. Windows also accepts
			// C:/like/this, but that form shows up mostly inside quoted error
			// text and tool output, and rewriting a device's own prose is worse
			// than leaving one uncommon spelling unmapped.
			root := regexp.QuoteMeta(renderLocalPath(localPath, kind, mp.windows))
			tail := `((?:` + separatorPattern(kind) + pathSegment + `)*)`
			// Boundary-aware: only replace the path when it is not followed by a
			// name character, so /Users/merv never matches inside /Users/mervynlally.
			boundary := `([^A-Za-z0-9_.-]|$)`
			mp.normRe[kind] = regexp.MustCompile(root + tail + boundary)
			mp.localIn[kind] = renderLocalPath(localPath, kind, mp.windows)
		}

		mp.resolveRe = regexp.MustCompile(regexp.QuoteMeta(pathToken(name)) + `((?:/` + pathSegment + `)*)`)

		m.mappings = append(m.mappings, mp)
		return nil
	}

	for localPath, name := range userMap {
		if strings.EqualFold(name, "HOME") {
			return nil, fmt.Errorf("path_map token HOME is reserved (the home directory is mapped automatically)")
		}
		if err := add(name, localPath); err != nil {
			return nil, err
		}
	}
	if homeDir != "" {
		if err := add("HOME", homeDir); err != nil {
			return nil, err
		}
	}

	// Longest local path first so ~/work maps to its own token before ~ does.
	sort.SliceStable(m.mappings, func(i, j int) bool {
		return len(m.mappings[i].localPath) > len(m.mappings[j].localPath)
	})

	return m, nil
}

// separatorPattern matches a path separator as it appears in this content kind:
// a forward slash, or a backslash spelled the way the format escapes it.
func separatorPattern(kind pathContentKind) string {
	if kind == pathContentJSON {
		return `(?:\\\\|/)`
	}
	return `(?:\\|/)`
}

// renderLocalPath spells an absolute local path for the given content kind.
func renderLocalPath(p string, kind pathContentKind, windows bool) string {
	if windows && kind == pathContentJSON {
		return strings.ReplaceAll(p, `\`, `\\`)
	}
	return p
}

// localSeparator is the separator to emit for this device and content kind.
func localSeparator(kind pathContentKind, windows bool) string {
	if !windows {
		return "/"
	}
	if kind == pathContentJSON {
		return `\\`
	}
	return `\`
}

// pathContentKindFor reports how the file at relPath spells paths. Conflict copies
// (path.conflict.<timestamp>) inherit the base path's format.
func pathContentKindFor(relPath string) pathContentKind {
	if i := strings.Index(relPath, ".conflict."); i >= 0 {
		relPath = relPath[:i]
	}
	switch path.Ext(relPath) {
	case ".json", ".jsonl":
		return pathContentJSON
	}
	return pathContentPlain
}

// EncodeClaudePath applies Claude Code's project directory encoding: every
// character outside [A-Za-z0-9] becomes "-".
func EncodeClaudePath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Tokens use the ${NAME} form in both remote keys and file content. This
// matches the format already written to existing buckets; note that a literal
// "${HOME}" in transcript content (e.g. a quoted shell snippet) is therefore
// indistinguishable from a normalized path and resolves to the local home
// directory on pull.
func pathToken(name string) string { return "${" + name + "}" }

const tokenPrefix = "${"

// splitProjectsPath splits "projects/<seg>/rest" into seg and "/rest".
// ok is false for paths not under projects/.
func splitProjectsPath(relPath string) (seg, rest string, ok bool) {
	const prefix = "projects/"
	if !strings.HasPrefix(relPath, prefix) {
		return "", "", false
	}
	remainder := relPath[len(prefix):]
	if i := strings.IndexByte(remainder, '/'); i >= 0 {
		return remainder[:i], remainder[i:], true
	}
	return remainder, "", true
}

// NormalizeRelPath rewrites a local relative path to its portable remote form.
// Only project directory segments are affected; everything else is unchanged.
// A nil mapper performs no translation (legacy behavior).
func (m *PathMapper) NormalizeRelPath(relPath string) string {
	if m == nil {
		return relPath
	}
	seg, rest, ok := splitProjectsPath(relPath)
	if !ok || strings.HasPrefix(seg, tokenPrefix) {
		return relPath
	}
	for _, mp := range m.mappings {
		if seg == mp.encLocal || strings.HasPrefix(seg, mp.encLocal+"-") {
			return "projects/" + pathToken(mp.name) + seg[len(mp.encLocal):] + rest
		}
	}
	return relPath
}

// ResolveRelPath rewrites a portable remote path back to a local relative
// path. ok is false when the path uses a token this device has no mapping
// for (the caller should skip the file and tell the user to extend path_map).
func (m *PathMapper) ResolveRelPath(relPath string) (string, bool) {
	if m == nil {
		return relPath, true
	}
	seg, rest, isProject := splitProjectsPath(relPath)
	if !isProject || !strings.HasPrefix(seg, tokenPrefix) {
		return relPath, true
	}
	for _, mp := range m.mappings {
		token := pathToken(mp.name)
		if strings.HasPrefix(seg, token) {
			return "projects/" + mp.encLocal + seg[len(token):] + rest, true
		}
	}
	return relPath, false
}

// NormalizeContent replaces this device's mapped path prefixes with portable
// tokens in the content of the file at relPath. Replacement is boundary-aware
// so one user's home path never matches inside a longer username, and the
// matched path tail is canonicalized to "/" separators so a Windows device and
// a POSIX device produce byte-identical remote content.
func (m *PathMapper) NormalizeContent(relPath string, data []byte) []byte {
	if m == nil {
		return data
	}
	kind := pathContentKindFor(relPath)
	for i := range m.mappings {
		mp := &m.mappings[i]
		re := mp.normRe[kind]
		token := []byte(pathToken(mp.name))
		data = re.ReplaceAllFunc(data, func(match []byte) []byte {
			sub := re.FindSubmatch(match)
			if sub == nil {
				return match
			}
			out := make([]byte, 0, len(token)+len(sub[1])+len(sub[2]))
			out = append(out, token...)
			out = append(out, canonicalizeTail(sub[1], kind)...)
			out = append(out, sub[2]...)
			return out
		})
	}
	return data
}

// ResolveContent replaces portable tokens with this device's local paths,
// rendering separators and escaping for the format of the file at relPath.
func (m *PathMapper) ResolveContent(relPath string, data []byte) []byte {
	if m == nil {
		return data
	}
	kind := pathContentKindFor(relPath)
	for i := range m.mappings {
		mp := &m.mappings[i]
		re := mp.resolveRe
		local := []byte(mp.localIn[kind])
		sep := []byte(localSeparator(kind, mp.windows))
		data = re.ReplaceAllFunc(data, func(match []byte) []byte {
			sub := re.FindSubmatch(match)
			if sub == nil {
				return match
			}
			tail := sub[1]
			if mp.windows {
				tail = bytes.ReplaceAll(tail, []byte("/"), sep)
			}
			out := make([]byte, 0, len(local)+len(tail))
			out = append(out, local...)
			out = append(out, tail...)
			return out
		})
	}
	return data
}

// canonicalizeTail rewrites the separators of a matched path tail to "/" so the
// remote form does not depend on which OS pushed it.
func canonicalizeTail(tail []byte, kind pathContentKind) []byte {
	if kind == pathContentJSON {
		return bytes.ReplaceAll(tail, []byte(`\\`), []byte("/"))
	}
	return bytes.ReplaceAll(tail, []byte(`\`), []byte("/"))
}

// IsPortableContentPath reports whether content path translation applies to
// this relative path: text formats under projects/ plus the prompt history.
// Conflict copies (path.conflict.<timestamp>) inherit the base path's rule.
func IsPortableContentPath(relPath string) bool {
	if i := strings.Index(relPath, ".conflict."); i >= 0 {
		relPath = relPath[:i]
	}
	if relPath == "history.jsonl" {
		return true
	}
	if !strings.HasPrefix(relPath, "projects/") {
		return false
	}
	switch path.Ext(relPath) {
	case ".jsonl", ".json", ".md", ".txt":
		return true
	}
	return false
}
