package filter

import (
	"container/list"
	"context"
	"math/bits"
	"regexp"
	"strings"
	"sync"

	pool "github.com/libp2p/go-buffer-pool"

	"github.com/rclone/rclone/fs"
)

const (
	listingBatchSize      = 64 // matches uint64 bit width
	listingBatchThreshold = 4  // below this, use scalar evaluation
)

type listingEvalStepKind uint8

const (
	listingEvalLeaf  listingEvalStepKind = iota // index into leafBlocks
	listingEvalRegex                            // index into regexBlocks
)

type listingEvalStep struct {
	kind  listingEvalStepKind
	index int // index into leafBlocks or regexBlocks depending on kind
}

// listingResidual holds the precomputed per-parent filter evaluation blocks.
// leafBlocks match by extension/dotfile status; regexBlocks match by full path regex.
// evalOrder is nil for sequential (all leaf blocks then all regex blocks) evaluation;
// when non-nil, it specifies an interleaved evaluation order.
// fallback is the default result when no block matches a leaf.
type listingResidual struct {
	leafBlocks  []listingLeafBlock
	regexBlocks []listingRegexBlock
	evalOrder   []listingEvalStep // non-nil only when leaf/regex blocks are interleaved
	fallback    bool              // default result when no block matches
}

type listingLeafBlock struct {
	include  bool
	tinyExts *tinySet // pointer to interned extension set
	dotfile  bool     // true means "match if leaf starts with '.'"
}

type listingRegexBlock struct {
	include  bool
	matchers []*regexp.Regexp // any-match: returns true if ANY matcher hits
}

// cachedResidual holds a precomputed listing residual for a specific parent
// directory, cached in the LRU. terminal is true when the parent analysis
// determined every child unconditionally matches, making per-leaf evaluation
// unnecessary.
type cachedResidual struct {
	residual   *listingResidual
	terminal   bool // true only when analysis.terminal && len(listingRules) == 0
	termResult bool // the fallback result; meaningful only when terminal == true
}

type flatListingCache struct {
	mu           sync.Mutex
	rules        []classifiedRule
	ignoreCase   bool
	tinyIntern   map[uint64][]*tinySet // cardinality bounded by distinct extension combos in filter rules
	maxResiduals int
	lruList      *list.List
	lruIndex     map[string]*list.Element
}

type lruEntry struct {
	parent string
	cr     *cachedResidual
}

type batchListingEvaluator struct {
	cache *flatListingCache // shared across calls; thread-safe via internal mutex
}

type residualAnalysis struct {
	terminal     bool           // true if a rule unconditionally matches ALL children
	listingRules []residualRule // surviving rules after dead-rule stripping
	fallback     bool           // default result when no surviving rule matches
}

type residualRule struct {
	rule          classifiedRule // the classified rule
	matchFullPath bool           // true: needs parent+leaf (regex block); false: leaf-only (leaf block)
}

type leafRuleAction int

const (
	leafRuleOK    leafRuleAction = iota // rule was added to leaf block
	leafRuleRegex                       // route to regex block instead
)

// leafExtension returns the file extension of a slash-free leaf name.
// leafExtension(".gitignore") returns ".gitignore" (intentional -- the entire
// filename is treated as the extension when the only dot is at position 0).
func leafExtension(leaf string) string {
	dot := strings.LastIndexByte(leaf, '.')
	if dot < 0 {
		return ""
	}
	return leaf[dot:]
}

// fillBool fills buf with value. Uses clear() for false (zero-value optimization).
func fillBool(buf []bool, value bool) {
	if !value {
		clear(buf)
		return
	}
	for i := range buf {
		buf[i] = true
	}
}

// match returns true if the leaf extension is in the tinySet OR (dotfile is
// enabled AND the leaf starts with '.').
func (b *listingLeafBlock) match(leafExt string, leafDot bool) bool {
	if leafExt != "" && b.tinyExts != nil && b.tinyExts.contains(leafExt) {
		return true
	}
	return b.dotfile && leafDot
}

// matchBytes tests the regex against a path constructed from parent + leaf as []byte.
// Uses go-buffer-pool to avoid string allocation.
func (rb *listingRegexBlock) matchBytes(parent string, leaf string) bool {
	// FIXME(perf): allocates per regex match; consider per-goroutine scratch buffer
	buf := pool.NewBuffer(nil)
	defer buf.Reset()
	_, _ = buf.WriteString(parent)
	_, _ = buf.WriteString(leaf)
	for _, re := range rb.matchers {
		if re != nil && re.Match(buf.Bytes()) {
			return true
		}
	}
	return false
}

// newFlatListingCache creates a flat listing cache from classified rules.
// Residuals are stored in an LRU capped at maxResiduals=10000, and each leaf
// block reuses interned tinySet pointers whose cardinality is bounded by the
// distinct extension combinations present in the filter rules.
func newFlatListingCache(rules []classifiedRule, ignoreCase bool) *flatListingCache {
	return &flatListingCache{
		rules:        rules,
		ignoreCase:   ignoreCase,
		tinyIntern:   make(map[uint64][]*tinySet),
		maxResiduals: 10000,
		lruList:      list.New(),
		lruIndex:     make(map[string]*list.Element),
	}
}

// internTinySet returns a shared *tinySet for the given set, reusing an
// existing interned instance if one with identical contents exists.
// Must be called under fc.mu.
func (fc *flatListingCache) internTinySet(ts *tinySet) *tinySet {
	h := ts.hash()
	for _, existing := range fc.tinyIntern[h] {
		if existing.equal(ts) {
			return existing
		}
	}
	fc.tinyIntern[h] = append(fc.tinyIntern[h], ts)
	return ts
}

// componentIterator yields path components from a normalized string without allocating.
type componentIterator struct {
	s   string
	pos int
}

// next returns the next non-empty component and true, or ("", false) at end.
func (it *componentIterator) next() (string, bool) {
	for it.pos < len(it.s) {
		start := it.pos
		slash := strings.IndexByte(it.s[start:], '/')
		if slash < 0 {
			it.pos = len(it.s)
			comp := it.s[start:]
			if comp != "" {
				return comp, true
			}
			return "", false
		}
		it.pos = start + slash + 1
		comp := it.s[start : start+slash]
		if comp != "" {
			return comp, true
		}
	}
	return "", false
}

// countComponents returns the number of non-empty path components.
func countComponents(s string) int {
	n := 0
	it := componentIterator{s: s}
	for {
		_, ok := it.next()
		if !ok {
			break
		}
		n++
	}
	return n
}

// normalizeParent ensures parent has a trailing slash (unless empty).
func normalizeParent(parent string) string {
	if parent == "" {
		return ""
	}
	if strings.HasSuffix(parent, "/") {
		return parent
	}
	return parent + "/"
}

// parentHasComponentStr iterates over a raw normalized parent string,
// checking if any component matches the predicate. Zero allocations.
func parentHasComponentStr(parent string, match func(string) bool) bool {
	it := componentIterator{s: parent}
	for {
		comp, ok := it.next()
		if !ok {
			return false
		}
		if match(comp) {
			return true
		}
	}
}

// analyzeParentResidual strips dead rules for a parent listing and detects
// terminal fallback outcomes that apply to every child in the listing.
func analyzeParentResidual(parent string, rules []classifiedRule) residualAnalysis {
	// FIXME(perf): rebuilds residual rule state per entry; cache per parent directory
	// INVARIANT: listing leaves are single path components (no slashes).
	// This function classifies rules for leaf-only evaluation. Rules that can
	// only match multi-component paths (for example, a deeper rooted prefix) are
	// dead for listing mode and silently dropped. Directory traversal handles
	// them via residualFor() calls at deeper parents.
	parent = normalizeParent(parent)
	nComps := countComponents(parent)
	analysis := residualAnalysis{
		fallback:     true,
		listingRules: make([]residualRule, 0, len(rules)),
	}

	for _, rule := range rules {
		switch rule.Kind {
		case fpMatchAll:
			analysis.fallback = rule.Include
			analysis.terminal = true
			return analysis

		case fpExtensionSet:
			analysis.listingRules = append(analysis.listingRules, residualRule{
				rule:          rule,
				matchFullPath: false,
			})

		case fpRootedExtension:
			if nComps == 0 {
				analysis.listingRules = append(analysis.listingRules, residualRule{
					rule:          rule,
					matchFullPath: false,
				})
			}

		case fpRootedPrefixSet:
			if nComps == 0 {
				// dead: listing leaves at root have no slash to match prefix/**
				continue
			}
			pit := componentIterator{s: parent}
			first, _ := pit.next()
			if _, ok := rule.Prefixes[first]; ok {
				analysis.fallback = rule.Include
				analysis.terminal = true
				return analysis
			}

		case fpRootedPrefix:
			nPrefixComps := countComponents(rule.Prefix)
			shared := min(nComps, nPrefixComps)
			pit := componentIterator{s: parent}
			prit := componentIterator{s: rule.Prefix}
			mismatch := false
			for range shared {
				pc, _ := pit.next()
				prc, _ := prit.next()
				if pc != prc {
					mismatch = true
					break
				}
			}
			if mismatch {
				continue
			}
			if nComps >= nPrefixComps {
				analysis.fallback = rule.Include
				analysis.terminal = true
				return analysis
			}
			// dead: listing leaves can't match deeper prefix

		case fpUnrootedPrefixSet:
			if parentHasComponentStr(parent, func(comp string) bool {
				_, ok := rule.Prefixes[comp]
				return ok
			}) {
				analysis.fallback = rule.Include
				analysis.terminal = true
				return analysis
			}
			// dead: listing leaves are single filenames without /

		case fpDotfileAll:
			if parentHasComponentStr(parent, func(comp string) bool {
				return len(comp) > 0 && comp[0] == '.'
			}) {
				analysis.fallback = rule.Include
				analysis.terminal = true
				return analysis
			}
			analysis.listingRules = append(analysis.listingRules, residualRule{
				rule:          rule,
				matchFullPath: false,
			})

		case fpUnclassified:
			analysis.listingRules = append(analysis.listingRules, residualRule{
				rule:          rule,
				matchFullPath: true,
			})

		default:
			analysis.listingRules = append(analysis.listingRules, residualRule{
				rule:          rule,
				matchFullPath: true,
			})
		}
	}

	return analysis
}

func classifyLeafRule(block *listingLeafBlock, tempExts map[string]struct{}, rule classifiedRule) leafRuleAction {
	// FIXME(perf): re-classifies rules per entry; cache classification per parent directory
	switch rule.Kind {
	case fpExtensionSet:
		for ext := range rule.Extensions {
			tempExts[ext] = struct{}{}
		}
		return leafRuleOK

	case fpRootedExtension:
		if strings.Count(rule.Extension, ".") != 1 {
			fs.Debugf(nil, "listing: multi-dot RootedExtension %q routed to regex fallback", rule.Extension)
			return leafRuleRegex
		}
		tempExts[rule.Extension] = struct{}{}
		return leafRuleOK

	case fpDotfileAll:
		block.dotfile = true
		return leafRuleOK

	default:
		fs.Debugf(nil, "listing: unsupported fastPathKind %d in leaf position, routing to regex", rule.Kind)
		return leafRuleRegex
	}
}

// buildListingResidual groups residual rules into leaf-only and regex blocks
// for batched evaluation of sibling entries in a directory listing.
func buildListingResidual(listingRules []residualRule, fallback bool) (*listingResidual, []map[string]struct{}) {
	residual := &listingResidual{fallback: fallback}
	if len(listingRules) == 0 {
		return residual, nil
	}

	residual.leafBlocks = make([]listingLeafBlock, 0, len(listingRules))
	residual.regexBlocks = make([]listingRegexBlock, 0)
	order := make([]listingEvalStep, 0, len(listingRules))
	leafExtMaps := make([]map[string]struct{}, 0, len(listingRules))

	seenRegex := false
	interleaved := false
	lastKind := listingEvalLeaf
	haveLast := false

	for _, rr := range listingRules {
		if rr.matchFullPath {
			seenRegex = true
			if haveLast && lastKind == listingEvalRegex &&
				residual.regexBlocks[len(residual.regexBlocks)-1].include == rr.rule.Include {
				block := &residual.regexBlocks[len(residual.regexBlocks)-1]
				block.matchers = append(block.matchers, rr.rule.Fallback)
				continue
			}

			residual.regexBlocks = append(residual.regexBlocks, listingRegexBlock{
				include:  rr.rule.Include,
				matchers: []*regexp.Regexp{rr.rule.Fallback},
			})
			order = append(order, listingEvalStep{
				kind:  listingEvalRegex,
				index: len(residual.regexBlocks) - 1,
			})
			lastKind = listingEvalRegex
			haveLast = true
			continue
		}

		if seenRegex {
			interleaved = true
		}

		if haveLast && lastKind == listingEvalLeaf &&
			residual.leafBlocks[len(residual.leafBlocks)-1].include == rr.rule.Include {
			lastLeafIdx := len(residual.leafBlocks) - 1
			action := classifyLeafRule(&residual.leafBlocks[lastLeafIdx], leafExtMaps[lastLeafIdx], rr.rule)
			if action == leafRuleRegex {
				seenRegex = true
				interleaved = true
				residual.regexBlocks = append(residual.regexBlocks, listingRegexBlock{
					include:  rr.rule.Include,
					matchers: []*regexp.Regexp{rr.rule.Fallback},
				})
				order = append(order, listingEvalStep{
					kind:  listingEvalRegex,
					index: len(residual.regexBlocks) - 1,
				})
				lastKind = listingEvalRegex
				continue
			}
			continue
		}

		newBlock := listingLeafBlock{include: rr.rule.Include}
		newExtMap := make(map[string]struct{})
		action := classifyLeafRule(&newBlock, newExtMap, rr.rule)
		if action == leafRuleRegex {
			seenRegex = true
			residual.regexBlocks = append(residual.regexBlocks, listingRegexBlock{
				include:  rr.rule.Include,
				matchers: []*regexp.Regexp{rr.rule.Fallback},
			})
			order = append(order, listingEvalStep{
				kind:  listingEvalRegex,
				index: len(residual.regexBlocks) - 1,
			})
			lastKind = listingEvalRegex
			haveLast = true
			continue
		}

		residual.leafBlocks = append(residual.leafBlocks, newBlock)
		leafExtMaps = append(leafExtMaps, newExtMap)
		order = append(order, listingEvalStep{
			kind:  listingEvalLeaf,
			index: len(residual.leafBlocks) - 1,
		})
		lastKind = listingEvalLeaf
		haveLast = true
	}

	if interleaved {
		residual.evalOrder = order
	}

	return residual, leafExtMaps
}

// residualFor returns the cached listing residual for a parent, building it on
// cache miss without holding the mutex across the expensive analysis step.
func (fc *flatListingCache) residualFor(parent string) *cachedResidual {
	// FIXME(perf): rebuilds tiny sets per batch call; cache parent-directory residual
	fc.mu.Lock()
	if elem, ok := fc.lruIndex[parent]; ok {
		fc.lruList.MoveToFront(elem)
		cr := elem.Value.(*lruEntry).cr
		fc.mu.Unlock()
		return cr
	}
	fc.mu.Unlock()

	analysis := analyzeParentResidual(parent, fc.rules)
	residual, leafExtMaps := buildListingResidual(analysis.listingRules, analysis.fallback)

	cr := &cachedResidual{residual: residual}
	if analysis.terminal && len(analysis.listingRules) == 0 {
		cr.terminal = true
		cr.termResult = analysis.fallback
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	if elem, ok := fc.lruIndex[parent]; ok {
		fc.lruList.MoveToFront(elem)
		return elem.Value.(*lruEntry).cr
	}

	for i := range residual.leafBlocks {
		if len(leafExtMaps[i]) > 0 {
			ts := tinySetFromMap(leafExtMaps[i])
			residual.leafBlocks[i].tinyExts = fc.internTinySet(&ts)
		}
	}

	if fc.maxResiduals > 0 && fc.lruList.Len() >= fc.maxResiduals {
		back := fc.lruList.Back()
		if back != nil {
			evictEntry := back.Value.(*lruEntry)
			fc.lruList.Remove(back)
			delete(fc.lruIndex, evictEntry.parent)
		}
	}

	entry := &lruEntry{parent: parent, cr: cr}
	elem := fc.lruList.PushFront(entry)
	fc.lruIndex[parent] = elem

	return cr
}

func evalLeafBlockBatch(block *listingLeafBlock, leaves []string) uint64 {
	if len(leaves) > listingBatchSize {
		panic("filter: listing chunk exceeds batch size")
	}

	var mask uint64
	for i, leaf := range leaves {
		if block.match(leafExtension(leaf), len(leaf) > 0 && leaf[0] == '.') {
			mask |= uint64(1) << i
		}
	}
	return mask
}

func applyBatchResults(results []bool, offset int, mask uint64, include bool) {
	for mask != 0 {
		bit := bits.TrailingZeros64(mask)
		results[offset+bit] = include
		mask &= mask - 1
	}
}

func scalarEvalResidual(r *listingResidual, parent string, leaves []string, results []bool) {
outer:
	for i, leaf := range leaves {
		leafExt := leafExtension(leaf)
		leafDot := len(leaf) > 0 && leaf[0] == '.'

		if len(r.evalOrder) == 0 {
			for j := range r.leafBlocks {
				if r.leafBlocks[j].match(leafExt, leafDot) {
					results[i] = r.leafBlocks[j].include
					continue outer
				}
			}
			for j := range r.regexBlocks {
				if r.regexBlocks[j].matchBytes(parent, leaf) {
					results[i] = r.regexBlocks[j].include
					continue outer
				}
			}
			continue
		}

		for _, step := range r.evalOrder {
			switch step.kind {
			case listingEvalLeaf:
				if r.leafBlocks[step.index].match(leafExt, leafDot) {
					results[i] = r.leafBlocks[step.index].include
					continue outer
				}
			case listingEvalRegex:
				if r.regexBlocks[step.index].matchBytes(parent, leaf) {
					results[i] = r.regexBlocks[step.index].include
					continue outer
				}
			}
		}
	}
}

// runListing evaluates a directory listing against the cached residual,
// amortizing parent analysis across sibling leaves and batching by 64 entries.
func (be *batchListingEvaluator) runListing(ctx context.Context, parent string, leaves []string, results []bool) {
	parent = normalizeParent(parent)
	cr := be.cache.residualFor(parent)
	if cr.terminal {
		fillBool(results, cr.termResult)
		return
	}

	r := cr.residual
	fillBool(results, r.fallback)
	if len(leaves) < listingBatchThreshold {
		scalarEvalResidual(r, parent, leaves, results)
		return
	}

	for offset := 0; offset < len(leaves); offset += listingBatchSize {
		if offset > 0 && ctx.Err() != nil {
			return
		}

		end := min(offset+listingBatchSize, len(leaves))
		chunk := leaves[offset:end]
		if len(chunk) > listingBatchSize {
			panic("filter: listing chunk exceeds batch size")
		}

		chunkSize := len(chunk)
		var allBits uint64
		if chunkSize == listingBatchSize {
			allBits = ^uint64(0)
		} else {
			allBits = (uint64(1) << chunkSize) - 1
		}

		resolved := uint64(0)
		if len(r.evalOrder) == 0 {
			for i := range r.leafBlocks {
				mask := evalLeafBlockBatch(&r.leafBlocks[i], chunk) &^ resolved
				applyBatchResults(results, offset, mask, r.leafBlocks[i].include)
				resolved |= mask
				if resolved == allBits {
					break
				}
			}
			if resolved != allBits {
				for i := range r.regexBlocks {
					var mask uint64
					for j := range chunk {
						bit := uint64(1) << j
						if resolved&bit != 0 {
							continue
						}
						if r.regexBlocks[i].matchBytes(parent, chunk[j]) {
							mask |= bit
						}
					}
					applyBatchResults(results, offset, mask, r.regexBlocks[i].include)
					resolved |= mask
					if resolved == allBits {
						break
					}
				}
			}
			continue
		}

		for _, step := range r.evalOrder {
			switch step.kind {
			case listingEvalLeaf:
				mask := evalLeafBlockBatch(&r.leafBlocks[step.index], chunk) &^ resolved
				applyBatchResults(results, offset, mask, r.leafBlocks[step.index].include)
				resolved |= mask
			case listingEvalRegex:
				var mask uint64
				for j := range chunk {
					bit := uint64(1) << j
					if resolved&bit != 0 {
						continue
					}
					if r.regexBlocks[step.index].matchBytes(parent, chunk[j]) {
						mask |= bit
					}
				}
				applyBatchResults(results, offset, mask, r.regexBlocks[step.index].include)
				resolved |= mask
			}
			if resolved == allBits {
				break
			}
		}
	}
}
