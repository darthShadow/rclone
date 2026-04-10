package filter

import "strings"

type fusedBlockKind int

const (
	fusedBlockSingle fusedBlockKind = iota
	fusedBlockUnrootedPrefixSets
	fusedBlockRootedPrefixSets
	fusedBlockMixedRooted
	fusedBlockExtensionDotfile
	fusedBlockMixed
)

type fusedBlock struct {
	include bool
	match   func(remote string, props pathProps) bool
}

type fusedRuleSet struct {
	blocks []fusedBlock
}

func (frs *fusedRuleSet) evaluate(remote string) bool {
	props := computePathProps(remote)
	for _, block := range frs.blocks {
		if block.match(remote, props) {
			return block.include
		}
	}
	return true
}

func buildFusedRuleSet(rules []classifiedRule) *fusedRuleSet {
	if len(rules) == 0 {
		return &fusedRuleSet{}
	}

	blocks := make([]fusedBlock, 0, len(rules))
	start := 0
	include := rules[0].Include
	for i := 1; i < len(rules); i++ {
		if rules[i].Include == include {
			continue
		}
		blocks = append(blocks, fuseTinySetBlock(rules[start:i], include))
		start = i
		include = rules[i].Include
	}
	blocks = append(blocks, fuseTinySetBlock(rules[start:], include))

	return &fusedRuleSet{blocks: blocks}
}

func classifyFusedBlock(rules []classifiedRule) fusedBlockKind {
	if len(rules) <= 1 {
		return fusedBlockSingle
	}

	allUnrootedPrefixSets := true
	allRootedPrefixSets := true
	allMixedRooted := true
	allExtensionDotfile := true

	for _, cr := range rules {
		switch cr.Kind {
		case fpUnrootedPrefixSet:
			allRootedPrefixSets = false
			allMixedRooted = false
			allExtensionDotfile = false
		case fpRootedPrefixSet:
			allUnrootedPrefixSets = false
			allMixedRooted = false
			allExtensionDotfile = false
		case fpRootedPrefix, fpRootedExtension:
			allUnrootedPrefixSets = false
			allRootedPrefixSets = false
			allExtensionDotfile = false
		case fpExtensionSet, fpDotfileAll:
			allUnrootedPrefixSets = false
			allRootedPrefixSets = false
			allMixedRooted = false
		default:
			allUnrootedPrefixSets = false
			allRootedPrefixSets = false
			allMixedRooted = false
			allExtensionDotfile = false
		}
	}

	switch {
	case allUnrootedPrefixSets:
		return fusedBlockUnrootedPrefixSets
	case allRootedPrefixSets:
		return fusedBlockRootedPrefixSets
	case allMixedRooted:
		return fusedBlockMixedRooted
	case allExtensionDotfile:
		return fusedBlockExtensionDotfile
	default:
		return fusedBlockMixed
	}
}

func fuseTinySetBlock(rules []classifiedRule, include bool) fusedBlock {
	if len(rules) == 0 {
		return fusedBlock{
			include: include,
			match: func(string, pathProps) bool {
				return false
			},
		}
	}

	switch classifyFusedBlock(rules) {
	case fusedBlockSingle:
		cr := rules[0]
		return fusedBlock{
			include: include,
			match: func(remote string, props pathProps) bool {
				return matchClassifiedRule(remote, props, cr)
			},
		}
	case fusedBlockUnrootedPrefixSets:
		return fuseTinySetUnrootedPrefixSets(rules, include)
	case fusedBlockRootedPrefixSets:
		return fuseTinySetRootedPrefixSets(rules, include)
	case fusedBlockMixedRooted:
		return fuseTinySetMixedRootedBlock(rules, include)
	case fusedBlockExtensionDotfile:
		return fuseTinySetExtensionDotfile(rules, include)
	default:
		return fuseTinySetMixedBlock(rules, include)
	}
}

func matchClassifiedRule(remote string, props pathProps, cr classifiedRule) bool {
	switch cr.Kind {
	case fpMatchAll:
		return true
	case fpRootedPrefix:
		if len(remote) <= len(cr.Prefix) || remote[len(cr.Prefix)] != '/' {
			return false
		}
		return strings.HasPrefix(remote, cr.Prefix)
	case fpRootedPrefixSet:
		if props.firstSlash < 0 {
			return false
		}
		_, ok := cr.Prefixes[props.firstComp]
		return ok
	case fpUnrootedPrefixSet:
		pos := 0
		for pos < len(remote) {
			slash := strings.IndexByte(remote[pos:], '/')
			if slash < 0 {
				return false
			}
			component := remote[pos : pos+slash]
			if _, ok := cr.Prefixes[component]; ok {
				return true
			}
			pos += slash + 1
		}
		return false
	case fpExtensionSet:
		if props.ext == "" {
			return false
		}
		_, ok := cr.Extensions[props.ext]
		return ok
	case fpRootedExtension:
		return props.firstSlash < 0 && props.ext == cr.Extension
	case fpDotfileAll:
		return props.hasDotComponent
	case fpUnclassified:
		return cr.Fallback != nil && cr.Fallback.MatchString(remote)
	default:
		return false
	}
}

func fuseTinySetRootedPrefixSets(rules []classifiedRule, include bool) fusedBlock {
	prefixes := make(map[string]struct{})
	for _, cr := range rules {
		for prefix := range cr.Prefixes {
			prefixes[prefix] = struct{}{}
		}
	}
	prefixSet := tinySetFromMap(prefixes)

	return fusedBlock{
		include: include,
		match: func(remote string, props pathProps) bool {
			if props.firstSlash < 0 {
				return false
			}
			return prefixSet.contains(props.firstComp)
		},
	}
}

func fuseTinySetUnrootedPrefixSets(rules []classifiedRule, include bool) fusedBlock {
	prefixes := make(map[string]struct{})
	for _, cr := range rules {
		for prefix := range cr.Prefixes {
			prefixes[prefix] = struct{}{}
		}
	}
	prefixSet := tinySetFromMap(prefixes)

	return fusedBlock{
		include: include,
		match: func(remote string, props pathProps) bool {
			pos := 0
			for pos < len(remote) {
				slash := strings.IndexByte(remote[pos:], '/')
				if slash < 0 {
					return false
				}
				component := remote[pos : pos+slash]
				if prefixSet.contains(component) {
					return true
				}
				pos += slash + 1
			}
			return false
		},
	}
}

func fuseTinySetMixedRootedBlock(rules []classifiedRule, include bool) fusedBlock {
	rootedExtensionsMap := make(map[string]struct{})
	directPrefixesMap := make(map[string]struct{})
	groupedRemainders := make(map[string]map[string]struct{})
	deepPrefixFallbacks := make(map[string][]classifiedRule)

	for _, cr := range rules {
		switch cr.Kind {
		case fpRootedExtension:
			rootedExtensionsMap[cr.Extension] = struct{}{}
		case fpRootedPrefix:
			first := cr.Prefix
			remainder := ""
			if slash := strings.IndexByte(cr.Prefix, '/'); slash >= 0 {
				first = cr.Prefix[:slash]
				remainder = cr.Prefix[slash+1:]
			}
			if remainder == "" {
				directPrefixesMap[first] = struct{}{}
				delete(groupedRemainders, first)
				delete(deepPrefixFallbacks, first)
				continue
			}
			if _, ok := directPrefixesMap[first]; ok {
				continue
			}
			if strings.Contains(remainder, "/") {
				deepPrefixFallbacks[first] = append(deepPrefixFallbacks[first], cr)
				continue
			}
			if groupedRemainders[first] == nil {
				groupedRemainders[first] = make(map[string]struct{})
			}
			groupedRemainders[first][remainder] = struct{}{}
		}
	}

	rootedExtensions := tinySetFromMap(rootedExtensionsMap)
	directPrefixes := tinySetFromMap(directPrefixesMap)
	groupedSets := make(map[string]tinySet, len(groupedRemainders))
	for first, remainders := range groupedRemainders {
		groupedSets[first] = tinySetFromMap(remainders)
	}

	return fusedBlock{
		include: include,
		match: func(remote string, props pathProps) bool {
			// root-level files: match by extension only
			if props.firstSlash < 0 {
				return props.ext != "" && rootedExtensions.contains(props.ext)
			}

			// single-component prefix match (e.g., /photos/**)
			if directPrefixes.contains(props.firstComp) {
				return true
			}

			// two-level prefix match (e.g., /photos/vacation/**)
			if remainders, ok := groupedSets[props.firstComp]; ok {
				rest := remote[props.firstSlash+1:]
				secondSlash := strings.IndexByte(rest, '/')
				if secondSlash >= 0 && remainders.contains(rest[:secondSlash]) {
					return true
				}
			}

			// deep prefix fallback via regexp (e.g., /a/b/c/**)
			for _, cr := range deepPrefixFallbacks[props.firstComp] {
				if cr.Fallback != nil && cr.Fallback.MatchString(remote) {
					return true
				}
			}

			return false
		},
	}
}

func fuseTinySetExtensionDotfile(rules []classifiedRule, include bool) fusedBlock {
	extensions := make(map[string]struct{})
	hasDotfile := false

	for _, cr := range rules {
		switch cr.Kind {
		case fpExtensionSet:
			for ext := range cr.Extensions {
				extensions[ext] = struct{}{}
			}
		case fpDotfileAll:
			hasDotfile = true
		}
	}
	extensionSet := tinySetFromMap(extensions)

	return fusedBlock{
		include: include,
		match: func(remote string, props pathProps) bool {
			if hasDotfile && props.hasDotComponent {
				return true
			}
			if props.ext == "" {
				return false
			}
			return extensionSet.contains(props.ext)
		},
	}
}

func fuseTinySetMixedBlock(rules []classifiedRule, include bool) fusedBlock {
	copied := append([]classifiedRule(nil), rules...)

	return fusedBlock{
		include: include,
		match: func(remote string, props pathProps) bool {
			for _, cr := range copied {
				if matchClassifiedRule(remote, props, cr) {
					return true
				}
			}
			return false
		},
	}
}
