package library

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/seanhamlin/tendarr/internal/clients/arr"
	"github.com/seanhamlin/tendarr/internal/clients/plex"
)

// Verifier confirms an *arr record refers to the same files Plex is showing.
//
// Matching on external id alone is not enough. A 4K library kept separately
// in Plex matches Radarr on IMDb id for every title, because it is the same
// film, but Radarr's record points at the 1080p copy in another folder.
// Deleting would remove the wrong file and free a fraction of the space the
// row advertises.
//
// Two independent checks, both of which must hold:
//
//  1. Section storage: does this *arr instance manage the folder this Plex
//     section reads from?
//  2. File identity: for movies, does Plex's file have the same name as the
//     file *arr says it manages?
//
// Anything unproven is unverified. A false negative costs a report-only row;
// a false positive deletes the wrong file.
type Verifier struct {
	// pathMap maps *arr container prefixes to Plex prefixes, longest first.
	pathMap []PathMapping
}

// PathMapping translates one *arr path prefix to the equivalent Plex prefix.
type PathMapping struct {
	Arr  string
	Plex string
}

// NewVerifier builds a verifier from parsed mappings.
func NewVerifier(mappings []PathMapping) *Verifier {
	m := append([]PathMapping(nil), mappings...)
	// Longest *arr prefix first, so /movies/4k wins over /movies.
	sort.SliceStable(m, func(i, j int) bool { return len(m[i].Arr) > len(m[j].Arr) })
	return &Verifier{pathMap: m}
}

// ParsePathMap parses one mapping per line, "arr-path:plex-path", e.g.
//
//	/movies:/mnt/media/movies
//	/tv:/mnt/media/tv
//
// Commas are also accepted as separators, matching the CLI's ARR_PATH_MAP.
func ParsePathMap(raw string) ([]PathMapping, error) {
	var out []PathMapping
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == ',' || r == '\r' })
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		parts := strings.SplitN(f, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("%q must look like /arr/path:/plex/path", f)
		}
		a, p := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if !strings.HasPrefix(a, "/") || !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("%q: both paths must be absolute", f)
		}
		out = append(out, PathMapping{Arr: a, Plex: p})
	}
	return out, nil
}

// Verify reports whether item manages the files plexItem refers to.
func (v *Verifier) Verify(section plex.Section, plexItem plex.Item, item arr.Item) bool {
	if match, known := v.FileMatches(plexItem, item); known && !match {
		return false
	}
	return v.SectionIsManagedBy(section, item)
}

// FileMatches compares file names. known is false when either side cannot
// say: series have no single file, and Radarr omits movieFile when nothing is
// downloaded.
func (v *Verifier) FileMatches(plexItem plex.Item, item arr.Item) (match, known bool) {
	if plexItem.FilePath == "" || item.FileRelativePath == "" {
		return false, false
	}
	return strings.EqualFold(baseName(plexItem.FilePath), baseName(item.FileRelativePath)), true
}

// SectionIsManagedBy checks whether the *arr root folder and the Plex section
// location refer to the same storage. Exact when a path map is configured;
// otherwise the last path segment is compared, which distinguishes /movies
// from /mnt/media/movies-uhd but fails safe on unrelated naming schemes.
func (v *Verifier) SectionIsManagedBy(section plex.Section, item arr.Item) bool {
	root := item.RootFolderPath
	if root == "" {
		root = parentOf(item.Path)
	}
	if root == "" || len(section.Locations) == 0 {
		return false
	}

	mapped, ok := v.translate(root)
	for _, loc := range section.Locations {
		if ok {
			if isWithin(loc, mapped) || isWithin(mapped, loc) {
				return true
			}
			continue
		}
		if strings.EqualFold(lastSegment(loc), lastSegment(root)) {
			return true
		}
	}
	return false
}

func (v *Verifier) translate(arrPath string) (string, bool) {
	for _, m := range v.pathMap {
		if isWithin(m.Arr, arrPath) {
			return strings.TrimRight(m.Plex, "/") + strings.TrimPrefix(strings.TrimRight(arrPath, "/"), strings.TrimRight(m.Arr, "/")), true
		}
	}
	return "", false
}

// isWithin reports whether p sits at or below prefix, on whole segments.
func isWithin(prefix, p string) bool {
	prefix = strings.TrimRight(prefix, "/")
	p = strings.TrimRight(p, "/")
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

func lastSegment(p string) string {
	return path.Base(strings.TrimRight(normaliseSlashes(p), "/"))
}

func parentOf(p string) string {
	parent := path.Dir(strings.TrimRight(normaliseSlashes(p), "/"))
	if parent == "." || parent == "/" || parent == "" {
		return ""
	}
	return parent
}

func baseName(p string) string {
	return path.Base(normaliseSlashes(p))
}

// normaliseSlashes copes with *arr running on Windows.
func normaliseSlashes(p string) string {
	return strings.ReplaceAll(p, `\`, "/")
}
