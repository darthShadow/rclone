package filter

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/rclone/rclone/fs"
)

// RulesOpt is configuration for a rule set
type RulesOpt struct {
	FilterRule  []string `config:"filter"`
	FilterFrom  []string `config:"filter_from"`
	ExcludeRule []string `config:"exclude"`
	ExcludeFrom []string `config:"exclude_from"`
	IncludeRule []string `config:"include"`
	IncludeFrom []string `config:"include_from"`
}

// rule is one filter rule
type rule struct {
	Include bool
	Regexp  *regexp.Regexp
	glob    string // post-mutation glob text (used by fast-path classification)
}

// Match returns true if rule matches path
func (r *rule) Match(path string) bool {
	return r.Regexp.MatchString(path)
}

// String the rule
func (r *rule) String() string {
	c := "-"
	if r.Include {
		c = "+"
	}
	return fmt.Sprintf("%s %s", c, r.Regexp.String())
}

// rules is a slice of rules
type rules struct {
	rules      []rule
	existing   map[string]struct{}
	fused      atomic.Pointer[fusedRuleSet]
	ignoreCase bool
}

type addFn func(Include bool, glob string) error

// add adds a rule if it doesn't exist already
func (rs *rules) add(Include bool, re *regexp.Regexp, glob string) {
	if rs.existing == nil {
		rs.existing = make(map[string]struct{})
	}
	newRule := rule{
		Include: Include,
		Regexp:  re,
		glob:    glob,
	}
	newRuleString := newRule.String()
	if _, ok := rs.existing[newRuleString]; ok {
		return // rule already exists
	}
	rs.rules = append(rs.rules, newRule)
	rs.existing[newRuleString] = struct{}{}
	rs.fused.Store(nil) // invalidate fused cache on mutation
}

// Add adds a filter rule with include or exclude status indicated
func (rs *rules) Add(Include bool, glob string) error {
	re, err := GlobPathToRegexp(glob, false /* f.Opt.IgnoreCase */)
	if err != nil {
		return err
	}
	rs.add(Include, re, glob)
	return nil
}

type clearFn func()

// clear clears all the rules
func (rs *rules) clear() {
	rs.rules = nil
	rs.existing = nil
	rs.fused.Store(nil)
}

// len returns the number of rules
func (rs *rules) len() int {
	return len(rs.rules)
}

// include returns whether this remote passes the filter rules.
func (rs *rules) include(remote string) bool {
	rs.ensureFused()
	return rs.fused.Load().evaluate(remote)
}

// ensureFused lazily builds the fusedRuleSet for fast-path evaluation.
// Thread-safe via atomic load/store. Double-build by concurrent goroutines
// is benign - fusedRuleSet is immutable; last store wins.
func (rs *rules) ensureFused() {
	if rs.fused.Load() != nil {
		return
	}
	// Build fused rule set with recovery
	var frs *fusedRuleSet
	func() {
		defer func() {
			if r := recover(); r != nil {
				fs.Errorf(nil, "filter fast-path build panic (falling back to linear scan): %v\n%s", r, debug.Stack())
				// Build fallback: one block per rule, preserving evaluation order
				// and polarity. This is equivalent to the pre-optimization linear scan.
				blocks := make([]fusedBlock, len(rs.rules))
				for i, r := range rs.rules {
					r := r // capture
					blocks[i] = fusedBlock{
						include: r.Include,
						match: func(remote string, _ pathProps) bool {
							return r.Regexp.MatchString(remote)
						},
					}
				}
				frs = &fusedRuleSet{blocks: blocks}
			}
		}()
		classified := make([]classifiedRule, len(rs.rules))
		for i, r := range rs.rules {
			classified[i] = classifyPattern(r.glob, r.Regexp, rs.ignoreCase)
			classified[i].Include = r.Include
			if classified[i].Kind == fpUnclassified && r.glob != "" {
				fs.Debugf(nil, "filter fast-path: unclassified pattern %q", r.glob)
			}
		}
		frs = buildFusedRuleSet(classified)
	}()
	rs.fused.Store(frs)
}

// include returns whether this collection of strings remote passes
// the filter rules.
//
// the first rule is evaluated on all the remotes and if it matches
// then the result is returned. If not the next rule is tested and so
// on.
func (rs *rules) includeMany(remotes []string) bool {
	for _, rule := range rs.rules {
		if slices.ContainsFunc(remotes, rule.Match) {
			return rule.Include
		}
	}
	return true
}

// scanNul is a split function for a Scanner that returns each NUL-terminated
// sequence of bytes. It correctly handles the final segment even if it
// lacks a trailing NUL.
func scanNul(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\x00'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// forEachLine calls fn on every line in the file pointed to by path
//
// It ignores empty lines and lines starting with '#' or ';' if raw is false
func forEachLine(path string, raw bool, useNulDelimiter bool, fn func(string) error) (err error) {
	var scanner *bufio.Scanner
	if path == "-" {
		scanner = bufio.NewScanner(os.Stdin)
	} else {
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		scanner = bufio.NewScanner(in)
		defer fs.CheckClose(in, &err)
	}

	if useNulDelimiter {
		scanner.Split(scanNul)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if !raw {
			line = strings.TrimSpace(line)
			if len(line) == 0 || line[0] == '#' || line[0] == ';' {
				continue
			}
		}
		err := fn(line)
		if err != nil {
			return err
		}
	}
	return scanner.Err()
}

// AddRule adds a filter rule with include/exclude indicated by the prefix
//
// These are
//
//	# Comment
//	+ glob
//	- glob
//	!
//
// '+' includes the glob, '-' excludes it and '!' resets the filter list
//
// Line comments may be introduced with '#' or ';'
func addRule(rule string, add addFn, clear clearFn) error {
	switch {
	case rule == "!":
		clear()
		return nil
	case strings.HasPrefix(rule, "- "):
		return add(false, rule[2:])
	case strings.HasPrefix(rule, "+ "):
		return add(true, rule[2:])
	}
	return fmt.Errorf("malformed rule %q", rule)
}

// AddRule adds a filter rule with include/exclude indicated by the prefix
//
// These are
//
//	# Comment
//	+ glob
//	- glob
//	!
//
// '+' includes the glob, '-' excludes it and '!' resets the filter list
//
// Line comments may be introduced with '#' or ';'
func (rs *rules) AddRule(rule string) error {
	return addRule(rule, rs.Add, rs.clear)
}

// Parse the rules passed in and add them to the function
func parseRules(opt *RulesOpt, add addFn, clear clearFn) (err error) {
	addImplicitExclude := false
	foundExcludeRule := false

	for _, rule := range opt.IncludeRule {
		err = add(true, rule)
		if err != nil {
			return err
		}
		addImplicitExclude = true
	}
	for _, rule := range opt.IncludeFrom {
		err := forEachLine(rule, false, false, func(line string) error {
			return add(true, line)
		})
		if err != nil {
			return err
		}
		addImplicitExclude = true
	}
	for _, rule := range opt.ExcludeRule {
		err = add(false, rule)
		if err != nil {
			return err
		}
		foundExcludeRule = true
	}
	for _, rule := range opt.ExcludeFrom {
		err := forEachLine(rule, false, false, func(line string) error {
			return add(false, line)
		})
		if err != nil {
			return err
		}
		foundExcludeRule = true
	}

	if addImplicitExclude && foundExcludeRule {
		fs.Errorf(nil, "Using --filter is recommended instead of both --include and --exclude as the order they are parsed in is indeterminate")
	}

	for _, rule := range opt.FilterRule {
		err = addRule(rule, add, clear)
		if err != nil {
			return err
		}
	}
	for _, rule := range opt.FilterFrom {
		err := forEachLine(rule, false, false, func(rule string) error {
			return addRule(rule, add, clear)
		})
		if err != nil {
			return err
		}
	}

	if addImplicitExclude {
		err = add(false, "/**")
		if err != nil {
			return err
		}
	}

	return nil
}
