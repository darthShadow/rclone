package filter

import (
	"regexp"
	"sort"
	"strings"
)

const tinySetThreshold = 8

type tinySet struct {
	inline [tinySetThreshold]string
	count  int
	m      map[string]struct{}
}

func newTinySet(elements []string) tinySet {
	var ts tinySet
	if len(elements) <= tinySetThreshold {
		copy(ts.inline[:], elements)
		ts.count = len(elements)
		sort.Strings(ts.inline[:ts.count])
		return ts
	}

	ts.count = -1
	ts.m = make(map[string]struct{}, len(elements))
	for _, element := range elements {
		ts.m[element] = struct{}{}
	}
	return ts
}

func tinySetFromMap(m map[string]struct{}) tinySet {
	elements := make([]string, 0, len(m))
	for element := range m {
		elements = append(elements, element)
	}
	return newTinySet(elements)
}

func (ts tinySet) contains(s string) bool {
	if ts.count >= 0 {
		for i := 0; i < ts.count; i++ {
			if ts.inline[i] == s {
				return true
			}
		}
		return false
	}

	_, ok := ts.m[s]
	return ok
}

// hash returns a deterministic hash of the tinySet contents.
// Inline elements are sorted at construction time (see newTinySet),
// so iteration order is already deterministic.
func (ts *tinySet) hash() uint64 {
	h := uint64(14695981039346656037) // FNV offset basis
	if ts.count >= 0 {
		for i := 0; i < ts.count; i++ {
			for _, b := range []byte(ts.inline[i]) {
				h ^= uint64(b)
				h *= 1099511628211 // FNV prime
			}
			h ^= uint64(0xFF) // separator
			h *= 1099511628211
		}
	} else {
		keys := make([]string, 0, len(ts.m))
		for k := range ts.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, b := range []byte(k) {
				h ^= uint64(b)
				h *= 1099511628211
			}
			h ^= uint64(0xFF)
			h *= 1099511628211
		}
	}
	return h
}

// equal returns true if two tinySets contain the same elements.
func (ts *tinySet) equal(other *tinySet) bool {
	if ts.count != other.count {
		return false
	}
	if ts.count >= 0 {
		for i := 0; i < ts.count; i++ {
			if ts.inline[i] != other.inline[i] {
				return false
			}
		}
		return true
	}
	if len(ts.m) != len(other.m) {
		return false
	}
	for k := range ts.m {
		if _, ok := other.m[k]; !ok {
			return false
		}
	}
	return true
}

type pathProps struct {
	firstSlash      int
	lastDot         int
	firstComp       string
	ext             string
	hasDotComponent bool
}

func computePathProps(remote string) pathProps {
	// FIXME(perf): re-derives path properties per entry; cache per-directory for sibling reuse
	pp := pathProps{firstSlash: -1, lastDot: -1}
	lastSlash := -1

	for i := 0; i < len(remote); i++ {
		switch remote[i] {
		case '/':
			if pp.firstSlash < 0 {
				pp.firstSlash = i
			}
			lastSlash = i
		case '.':
			pp.lastDot = i
			if i == 0 || remote[i-1] == '/' {
				pp.hasDotComponent = true
			}
		}
	}

	if pp.firstSlash >= 0 {
		pp.firstComp = remote[:pp.firstSlash]
	} else {
		pp.firstComp = remote
	}
	if pp.lastDot > lastSlash {
		pp.ext = remote[pp.lastDot:]
	}

	return pp
}

// fastPathKind identifies the structural category of a classified glob pattern.
type fastPathKind uint8

// fastPathKind values identify the supported structural fast-path categories.
const (
	fpUnclassified      fastPathKind = iota // fallback to compiled regexp
	fpRootedPrefix                          // /prefix/**
	fpRootedPrefixSet                       // /{a,b,c}/**
	fpUnrootedPrefixSet                     // {a,b,c}/**
	fpExtensionSet                          // *.{ext1,ext2} or *.ext
	fpRootedExtension                       // /*.ext
	fpDotfileAll                            // .**
	fpMatchAll                              // ** or /**
)

// classifiedRule holds the result of classifying a glob pattern.
// Kind determines which fields are populated:
//
//	fpRootedPrefix      → Prefix
//	fpRootedPrefixSet   → Prefixes
//	fpUnrootedPrefixSet → Prefixes
//	fpExtensionSet      → Extensions
//	fpRootedExtension   → Extension
//	fpDotfileAll        → (no data fields)
//	fpMatchAll          → (no data fields)
//	fpUnclassified      → Fallback (regexp used for matching)
//
// Fallback is set for ALL rules (used by mixed-block fallback evaluation).
// Include is set by the caller after classification, not by classifyPattern.
type classifiedRule struct {
	Kind       fastPathKind
	Include    bool
	Prefix     string
	Prefixes   map[string]struct{}
	Extensions map[string]struct{}
	Extension  string
	Fallback   *regexp.Regexp
	Glob       string
}

func classifyPattern(glob string, re *regexp.Regexp, ignoreCase bool) classifiedRule {
	cr := classifiedRule{
		Kind:     fpUnclassified,
		Fallback: re,
		Glob:     glob,
	}

	if ignoreCase {
		return cr
	}

	if glob == "**" || glob == "/**" {
		cr.Kind = fpMatchAll
		return cr
	}

	if glob == ".**" {
		cr.Kind = fpDotfileAll
		return cr
	}

	if strings.HasPrefix(glob, "/") && strings.HasSuffix(glob, "/**") &&
		!strings.ContainsAny(glob[:len(glob)-3], "{*?[\\") {
		prefix := glob[1 : len(glob)-3]
		if len(prefix) == 0 || prefix[len(prefix)-1] != '/' {
			cr.Kind = fpRootedPrefix
			cr.Prefix = prefix
			return cr
		}
	}

	if strings.HasPrefix(glob, "/{") && strings.HasSuffix(glob, "}/**") {
		inner := glob[2 : len(glob)-4]
		if !strings.ContainsAny(inner, "{*?[/\\") {
			parts := strings.Split(inner, ",")
			cr.Kind = fpRootedPrefixSet
			cr.Prefixes = make(map[string]struct{}, len(parts))
			for _, part := range parts {
				cr.Prefixes[part] = struct{}{}
			}
			return cr
		}
	}

	if strings.HasPrefix(glob, "*.") && !strings.HasPrefix(glob, "**") {
		rest := glob[2:]
		if strings.HasPrefix(rest, "{") && strings.HasSuffix(rest, "}") {
			inner := rest[1 : len(rest)-1]
			if !strings.ContainsAny(inner, "{*?[/.\\") {
				exts := strings.Split(inner, ",")
				cr.Kind = fpExtensionSet
				cr.Extensions = make(map[string]struct{}, len(exts))
				for _, ext := range exts {
					cr.Extensions["."+ext] = struct{}{}
				}
				return cr
			}
		} else if !strings.ContainsAny(rest, "{*?[/,.\\") {
			cr.Kind = fpExtensionSet
			cr.Extensions = map[string]struct{}{
				"." + rest: {},
			}
			return cr
		}
	}

	if strings.HasPrefix(glob, "/*.") && !strings.ContainsAny(glob[3:], "{*?[/.\\") {
		cr.Kind = fpRootedExtension
		cr.Extension = glob[2:]
		return cr
	}

	return classifyUnrooted(glob, cr)
}

func classifyUnrooted(glob string, cr classifiedRule) classifiedRule {
	if cr.Kind != fpUnclassified {
		return cr
	}
	if !strings.HasPrefix(glob, "{") || !strings.HasSuffix(glob, "}/**") {
		return cr
	}

	inner := glob[1 : len(glob)-4]
	if inner == "" || strings.ContainsAny(inner, "{*?[\\") {
		return cr
	}

	parts := strings.Split(inner, ",")
	for _, part := range parts {
		if part == "" || strings.Contains(part, "/") {
			return cr
		}
	}

	cr.Kind = fpUnrootedPrefixSet
	cr.Prefixes = make(map[string]struct{}, len(parts))
	for _, part := range parts {
		cr.Prefixes[part] = struct{}{}
	}
	return cr
}
