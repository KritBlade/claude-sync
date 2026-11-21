package main

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Synced content replaces each mapped folder with a portable token, and each device puts its own folder back.
// The token is deliberately unlike shell syntax, so a "${HOME}" quoted in a transcript is never rewritten.
const (
	portableTokenPrefix = "${PORTABLE_PATH:"
	portableTokenSuffix = "}"
	fileURIPrefix       = "file://"
)

var (
	tokenNamePattern     = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	portableTokenPattern = regexp.MustCompile(`\$\{PORTABLE_PATH:([A-Z][A-Z0-9_]*)\}`)
)

// pathRoot is one mapped folder on this device, such as WORK = H:\Github\Dev or HOME = /Users/goten.
type pathRoot struct {
	tokenName   string
	isWindows   bool
	driveLetter string   // Windows only, as configured
	components  []string // folder names after the drive (Windows) or after the leading "/" (POSIX)
	finder      *regexp.Regexp
}

// pathConverter rewrites this device's mapped folders to portable tokens and back. A folder is recognised in
// every spelling Antigravity writes: raw (H:\Github\Dev), escaped once or more inside JSON (H:\\Github\\Dev),
// with forward slashes (H:/Github/Dev), and as a file URI (file:///h%3A/Github/Dev, file:///h:/Github/Dev).
type pathConverter struct {
	roots      []*pathRoot
	rootByName map[string]*pathRoot
}

// newPathConverter maps the home folder to HOME and every path_map entry (local folder -> token name) to its
// token, as claude-sync does for Claude sessions.
func newPathConverter(homeDirectory string, pathMap map[string]string) (*pathConverter, error) {
	converter := &pathConverter{rootByName: map[string]*pathRoot{}}
	add := func(tokenName, localPath string) error {
		if !tokenNamePattern.MatchString(tokenName) {
			return fmt.Errorf("path_map token %q: use uppercase letters, digits and underscores", tokenName)
		}
		if _, isTaken := converter.rootByName[tokenName]; isTaken {
			return fmt.Errorf("path_map token %s is mapped to more than one folder", tokenName)
		}
		root, err := parsePathRoot(tokenName, localPath)
		if err != nil {
			return err
		}
		converter.roots = append(converter.roots, root)
		converter.rootByName[tokenName] = root
		return nil
	}
	for localPath, tokenName := range pathMap {
		if strings.EqualFold(tokenName, "HOME") {
			return nil, fmt.Errorf("path_map token HOME is reserved for the home folder")
		}
		if err := add(tokenName, localPath); err != nil {
			return nil, err
		}
	}
	if homeDirectory != "" {
		if err := add("HOME", homeDirectory); err != nil {
			return nil, err
		}
	}
	// The most specific folder wins where two match at one place (WORK = /Users/goten/Github/Dev over HOME).
	sort.Slice(converter.roots, func(i, j int) bool {
		return len(converter.roots[i].components) > len(converter.roots[j].components)
	})
	return converter, nil
}

func parsePathRoot(tokenName, localPath string) (*pathRoot, error) {
	root := &pathRoot{tokenName: tokenName}
	switch {
	case len(localPath) >= 2 && localPath[1] == ':' && isASCIILetter(localPath[0]):
		root.isWindows = true
		root.driveLetter = localPath[:1]
		root.components = splitPathComponents(localPath[2:], `\/`)
	case strings.HasPrefix(localPath, "/"):
		root.components = splitPathComponents(localPath, "/")
	default:
		return nil, fmt.Errorf("folder %q for token %s is not an absolute path", localPath, tokenName)
	}
	if len(root.components) == 0 {
		return nil, fmt.Errorf("folder %q for token %s is a drive or file-system root, which cannot be mapped", localPath, tokenName)
	}
	root.finder = regexp.MustCompile(root.finderPattern())
	return root, nil
}

func splitPathComponents(path, separators string) []string {
	var components []string
	for _, component := range strings.FieldsFunc(path, func(r rune) bool { return strings.ContainsRune(separators, r) }) {
		components = append(components, component)
	}
	return components
}

// finderPattern matches the folder itself, without the path below it. Group 1 is the file-URI prefix; on
// Windows, group 2+i is the separator before component i, so the caller can require one escaping depth.
func (root *pathRoot) finderPattern() string {
	var pattern strings.Builder
	if root.isWindows {
		pattern.WriteString(`(?i)(file:///)?` + regexp.QuoteMeta(root.driveLetter) + `(?::|%3a)`)
		for _, component := range root.components {
			pattern.WriteString(`(\\+|/)` + componentPattern(component))
		}
		return pattern.String()
	}
	pattern.WriteString(`(file://)?`)
	for _, component := range root.components {
		pattern.WriteString(`/` + componentPattern(component))
	}
	return pattern.String()
}

// componentPattern accepts a folder name as written or percent-encoded, as it appears inside a file URI.
func componentPattern(component string) string {
	escaped := url.PathEscape(component)
	if escaped == component {
		return regexp.QuoteMeta(component)
	}
	return `(?:` + regexp.QuoteMeta(component) + `|` + regexp.QuoteMeta(escaped) + `)`
}

// rawForm is how this device's folder is written back into content outside a URI. Windows folders use forward
// slashes: they work in every Windows path API and need no escaping inside JSON, where a backslash would.
func (root *pathRoot) rawForm() string {
	if root.isWindows {
		return root.driveLetter + ":/" + strings.Join(root.components, "/")
	}
	return "/" + strings.Join(root.components, "/")
}

// uriForm is how this device's folder is written back inside a file URI, matching the form VS Code and
// Antigravity record for a workspace (file:///h%3A/Github/Dev on Windows).
func (root *pathRoot) uriForm() string {
	escapedComponents := make([]string, len(root.components))
	for i, component := range root.components {
		escapedComponents[i] = url.PathEscape(component)
	}
	if root.isWindows {
		return fileURIPrefix + "/" + strings.ToLower(root.driveLetter) + "%3A/" + strings.Join(escapedComponents, "/")
	}
	return fileURIPrefix + "/" + strings.Join(escapedComponents, "/")
}

func (root *pathRoot) token() string {
	return portableTokenPrefix + root.tokenName + portableTokenSuffix
}

// rootMatch is one occurrence of a mapped folder, with the path below it, and its portable replacement.
type rootMatch struct {
	start, end  int
	specificity int
	replacement []byte
}

// toPortable replaces every occurrence of this device's mapped folders with portable tokens. The path below
// the folder keeps its names and has its separators turned into "/". It returns the number of replacements.
func (converter *pathConverter) toPortable(data []byte) ([]byte, int) {
	matches := converter.findLocalRoots(data)
	if len(matches) == 0 {
		return data, 0
	}
	out := make([]byte, 0, len(data))
	position := 0
	for _, match := range matches {
		out = append(out, data[position:match.start]...)
		out = append(out, match.replacement...)
		position = match.end
	}
	return append(out, data[position:]...), len(matches)
}

// containsLocalRoot reports whether toPortable would change data.
func (converter *pathConverter) containsLocalRoot(data []byte) bool {
	for _, root := range converter.roots {
		if root.finder.Match(data) {
			return len(converter.findLocalRoots(data)) > 0
		}
	}
	return false
}

// findLocalRoots returns non-overlapping matches in order. Where two folders match at one place, the more
// specific one is kept.
func (converter *pathConverter) findLocalRoots(data []byte) []rootMatch {
	var candidates []rootMatch
	for _, root := range converter.roots {
		for _, location := range root.finder.FindAllSubmatchIndex(data, -1) {
			if match, isValid := root.validateMatch(data, location); isValid {
				candidates = append(candidates, match)
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].start != candidates[j].start {
			return candidates[i].start < candidates[j].start
		}
		return candidates[i].specificity > candidates[j].specificity
	})
	var matches []rootMatch
	position := 0
	for _, candidate := range candidates {
		if candidate.start < position {
			continue
		}
		matches = append(matches, candidate)
		position = candidate.end
	}
	return matches
}

// validateMatch rejects a finder hit that is part of a longer name, mixes escaping depths, or is a URI with
// backslashes; for a valid one it reads the path below the folder and builds the replacement.
func (root *pathRoot) validateMatch(data []byte, location []int) (rootMatch, bool) {
	start, end := location[0], location[1]
	isURI := location[2] >= 0
	if !isURI && start > 0 && root.isPrecededByNameCharacter(data[start-1]) {
		return rootMatch{}, false
	}
	separator := []byte("/")
	if root.isWindows {
		for i := range root.components {
			group := 2 + i
			componentSeparator := data[location[2*group]:location[2*group+1]]
			if i == 0 {
				separator = componentSeparator
			} else if !bytes.Equal(componentSeparator, separator) {
				return rootMatch{}, false
			}
		}
		if isURI && !bytes.Equal(separator, []byte("/")) {
			return rootMatch{}, false
		}
	}
	if end < len(data) && isNameContinuation(data[end]) {
		return rootMatch{}, false
	}
	tail, tailEnd := scanPathTail(data, end, separator)
	replacement := make([]byte, 0, len(fileURIPrefix)+len(root.token())+len(tail))
	if isURI {
		replacement = append(replacement, fileURIPrefix...)
	}
	replacement = append(replacement, root.token()...)
	replacement = append(replacement, tail...)
	return rootMatch{start: start, end: tailEnd, specificity: len(root.components), replacement: replacement}, true
}

func (root *pathRoot) isPrecededByNameCharacter(previous byte) bool {
	if isASCIILetter(previous) || isASCIIDigit(previous) || previous == '_' {
		return true
	}
	// A POSIX folder must not match inside a longer path or a drive path (C:/Users/goten).
	return !root.isWindows && strings.IndexByte(`.~-/\:`, previous) >= 0
}

func isNameContinuation(next byte) bool {
	return isASCIILetter(next) || isASCIIDigit(next) || strings.IndexByte(`_.-%`, next) >= 0
}

// scanPathTail reads the path below a matched folder: separators of the folder's own escaping depth (or "/")
// each followed by a name. A shallower backslash run is not a separator, so the "\n" escape after a path
// inside JSON is never read as one. The tail is returned with "/" separators.
func scanPathTail(data []byte, position int, separator []byte) ([]byte, int) {
	var tail []byte
	isBackslashSeparator := separator[0] == '\\'
	for position < len(data) {
		next := position
		switch {
		case bytes.HasPrefix(data[position:], separator) &&
			!(isBackslashSeparator && position+len(separator) < len(data) && data[position+len(separator)] == '\\'):
			next = position + len(separator)
		case data[position] == '/':
			next = position + 1
		default:
			return tail, position
		}
		segmentEnd := next
		for segmentEnd < len(data) && isPathSegmentByte(data[segmentEnd]) {
			segmentEnd++
		}
		if segmentEnd == next {
			return tail, position
		}
		tail = append(tail, '/')
		tail = append(tail, data[next:segmentEnd]...)
		position = segmentEnd
	}
	return tail, position
}

func isPathSegmentByte(b byte) bool {
	if b < 0x20 || b == 0x7f || b == ' ' {
		return false
	}
	return strings.IndexByte(`\/"'<>|:*?`, b) < 0
}

// toLocal replaces portable tokens with this device's folders. It returns the number of tokens this device
// has no folder for; those are left as they are.
func (converter *pathConverter) toLocal(data []byte) ([]byte, int) {
	locations := portableTokenPattern.FindAllSubmatchIndex(data, -1)
	if len(locations) == 0 {
		return data, 0
	}
	out := make([]byte, 0, len(data))
	position := 0
	unknownTokens := 0
	for _, location := range locations {
		root, isMapped := converter.rootByName[string(data[location[2]:location[3]])]
		if !isMapped {
			unknownTokens++
			continue
		}
		start := location[0]
		replacement := root.rawForm()
		if start >= len(fileURIPrefix) && string(data[start-len(fileURIPrefix):start]) == fileURIPrefix {
			start -= len(fileURIPrefix)
			replacement = root.uriForm()
		}
		out = append(out, data[position:start]...)
		out = append(out, replacement...)
		position = location[1]
	}
	return append(out, data[position:]...), unknownTokens
}

func containsPortableToken(data []byte) bool {
	return bytes.Contains(data, []byte(portableTokenPrefix))
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }
func isASCIIDigit(b byte) bool  { return b >= '0' && b <= '9' }
