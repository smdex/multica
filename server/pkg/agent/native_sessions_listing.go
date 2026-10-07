package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	nativeSessionMaxDirectoryEntries = 5000
	nativeSessionMaxScanBytes        = 8 << 20
	nativeSessionMaxDepth            = 4
)

type nativeFileCandidate struct {
	path    string
	updated time.Time
}

// nativeSessionCandidates enumerates names and filesystem metadata only. It
// neither follows symlinks nor reads a transcript; the rooted open in
// readNativeFileHeader remains the authority before a candidate is displayed.
func nativeSessionCandidates(roots []string, suffix string) ([]nativeFileCandidate, bool) {
	var candidates []nativeFileCandidate
	entries := 0
	truncated := false
	for _, root := range roots {
		if truncated {
			break
		}
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			entries++
			if entries > nativeSessionMaxDirectoryEntries {
				truncated = true
				return filepath.SkipAll
			}
			if path == root {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			depth := strings.Count(rel, string(filepath.Separator)) + 1
			if depth > nativeSessionMaxDepth {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), suffix) {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			candidates = append(candidates, nativeFileCandidate{path: path, updated: info.ModTime().UTC()})
			return nil
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].updated.Equal(candidates[j].updated) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].updated.After(candidates[j].updated)
	})
	return candidates, truncated
}

// listNativeFileSessions advances the cursor over candidates, not hydrated
// sessions. A malformed header consumes one candidate and cannot make an
// unbounded retry loop. Every request has independent candidate and byte caps.
func listNativeFileSessions(roots []string, opts NativeSessionListOptions, parse func(nativeFile) (NativeSessionSummary, error)) (NativeSessionPage, error) {
	return listNativeFileSessionsWhere(roots, opts, nil, parse)
}

func listNativeFileSessionsWhere(roots []string, opts NativeSessionListOptions, eligible func(string) bool, parse func(nativeFile) (NativeSessionSummary, error)) (NativeSessionPage, error) {
	offset, err := parseNativeSessionCursor(opts.Cursor)
	if err != nil {
		return NativeSessionPage{}, err
	}
	candidates, enumerationTruncated := nativeSessionCandidates(roots, ".jsonl")
	if offset > len(candidates) {
		return NativeSessionPage{}, nativeSessionError(NativeSessionInvalidCursor, "cursor is outside the bounded native catalog")
	}
	limit := nativeSessionLimit(opts.Limit)
	page := NativeSessionPage{Truncated: enumerationTruncated}
	bytesRead := 0
	inspected := 0
	index := offset
	for index < len(candidates) && len(page.Sessions) < limit && inspected < nativeSessionMaxFiles {
		if eligible != nil && !eligible(candidates[index].path) {
			index++
			continue
		}
		if bytesRead+nativeSessionHeaderBytes > nativeSessionMaxScanBytes {
			page.Truncated = true
			break
		}
		candidate := candidates[index]
		index++
		inspected++
		// Reserve the complete bounded header read before opening. A malformed
		// candidate still consumed up to 64 KiB from disk and must not evade the
		// request-wide budget by returning no nativeFile payload.
		bytesRead += nativeSessionHeaderBytes
		header, readErr := readNativeFileHeader(candidate.path, roots)
		if readErr != nil {
			continue
		}
		summary, parseErr := parse(header)
		if parseErr != nil {
			continue
		}
		page.Sessions = append(page.Sessions, summary)
	}
	if index < len(candidates) {
		page.NextCursor = nativeSessionCursor(index)
		if inspected >= nativeSessionMaxFiles || bytesRead >= nativeSessionMaxScanBytes {
			page.Truncated = true
		}
	}
	return page, nil
}
