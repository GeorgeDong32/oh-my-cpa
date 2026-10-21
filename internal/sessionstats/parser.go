package sessionstats

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Parser converts one client's session storage into UnifiedEntry streams.
// Implementations stream via emit and never materialize whole files.
type Parser interface {
	// Client is the stable client id on every entry ("claude-code", "codex").
	Client() string
	// Roots lists the absolute data roots for this client under home; a
	// missing root is not an error.
	Roots(home string) []string
	// Discover lists source files under root. Files are the unit of
	// watermarking.
	Discover(root string) ([]string, error)
	// ParseFile streams the file's entries; parseErr marks the file
	// unreadable for the scan status. It is called only when the file changed.
	ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error
}

// WalkJSONLFiles lists *.jsonl files under root, sorted for deterministic
// scans; a missing root returns an empty list.
func WalkJSONLFiles(root string) ([]string, error) {
	return walkFiles(root, ".jsonl")
}

// WalkFilesWithSuffix lists files ending in suffix under root.
func WalkFilesWithSuffix(root, suffix string) ([]string, error) {
	return walkFiles(root, suffix)
}

func walkFiles(root, suffix string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	var files []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // unreadable subtrees are skipped, not fatal
		}
		if entry.IsDir() {
			return nil
		}
		if suffix == "" || strings.HasSuffix(entry.Name(), suffix) {
			files = append(files, path)
		}
		return nil
	})
	// Deterministic order keeps scans and read-time dedup winners stable.
	sort.Strings(files)
	return files, nil
}

// FileMtimeMS is the last-modified fallback timestamp parsers use when an
// entry carries none; 0 when unreadable.
func FileMtimeMS(file string) int64 {
	info, err := os.Stat(file)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixMilli()
}

// readFileLimited reads a file with the scan's hard size cap; ok is false for
// missing or oversized files (the caller marks them, never crashes).
func readFileLimited(file string, cap int64) ([]byte, bool) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, false
	}
	if info.Size() > cap {
		return nil, false
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	return body, true
}

// forEachJSONLine reads a capped file and hands each non-empty line's raw
// bytes to visit, tolerating a UTF-8 BOM and CRLF endings.
func forEachJSONLine(file string, mtimeMS int64, visit func(raw []byte)) {
	forEachJSONLineIndexed(file, mtimeMS, func(raw []byte, _ int) { visit(raw) })
}

// forEachJSONLineIndexed additionally reports the line's ordinal, for dedup
// keys that need a within-file discriminator.
func forEachJSONLineIndexed(file string, mtimeMS int64, visit func(raw []byte, index int)) {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return
	}
	_ = mtimeMS
	index := 0
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if index == 0 {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		visit(line, index)
		index++
	}
}

// forEachTextLine hands each text line (BOM/CRLF tolerated) to visit.
func forEachTextLine(file string, mtimeMS int64, visit func(line string)) {
	body, ok := readFileLimited(file, maxFileBytes)
	if !ok {
		return
	}
	_ = mtimeMS
	text := strings.TrimPrefix(string(body), "\xef\xbb\xbf")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		visit(line)
	}
}

// clampNonNeg floors a value at zero.
func clampNonNeg(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func sortStrings(values []string) {
	sort.Strings(values)
}
