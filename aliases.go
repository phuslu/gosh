package gosh

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/syntax"
)

// aliasTable mirrors the aliases defined in the interpreter, whose own table
// is private. mvdan.cc/sh expands only one level of aliases when it runs a
// command: with alias ll='ls -lF' and alias ls='ls --color=auto', "ll" runs
// a plain "ls -lF". Bash keeps expanding the first word until it reaches a
// word that is not an alias or an alias already being expanded, so gosh
// expands statements itself before handing them to the interpreter.
//
// The table follows the "alias" and "unalias" calls the interpreter sees,
// including ones in subshells, whose definitions Bash would drop when the
// subshell ends. Code that the interpreter parses on its own, such as eval
// strings and sourced files, still gets the interpreter's single level.
type aliasTable struct {
	mu   sync.RWMutex
	defs map[string]aliasDef
}

type aliasDef struct {
	words []*syntax.Word
	// blank reports whether the value ends in a blank, in which case the
	// word after it is checked for alias expansion too.
	blank bool
}

func newAliasTable() *aliasTable {
	return &aliasTable{defs: make(map[string]aliasDef)}
}

// define records the name=value arguments of an alias call the same way the
// interpreter's builtin parses them; arguments without "=" only print.
func (t *aliasTable) define(args []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
args:
	for _, arg := range args {
		name, src, ok := strings.Cut(arg, "=")
		if !ok {
			continue
		}
		var words []*syntax.Word
		for w, err := range syntax.NewParser().WordsSeq(strings.NewReader(src)) {
			if err != nil {
				continue args
			}
			words = append(words, w)
		}
		t.defs[name] = aliasDef{
			words: words,
			blank: strings.TrimRight(src, " \t") != src,
		}
	}
}

func (t *aliasTable) remove(names []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, name := range names {
		delete(t.defs, name)
	}
}

func (t *aliasTable) names() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return slices.Sorted(maps.Keys(t.defs))
}

func (t *aliasTable) reset() {
	t.mu.Lock()
	clear(t.defs)
	t.mu.Unlock()
}

// expand rewrites the command words of every simple command in node with
// Bash's recursive alias expansion. The interpreter then finds no alias left
// to expand in the command position.
func (t *aliasTable) expand(node syntax.Node) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.defs) == 0 {
		return
	}
	// Collect the calls before rewriting any, so the walk never descends into
	// words shared with the alias definitions.
	var calls []*syntax.CallExpr
	syntax.Walk(node, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok && len(call.Args) > 0 {
			calls = append(calls, call)
		}
		return true
	})
	for _, call := range calls {
		if _, ok := t.defs[call.Args[0].Lit()]; ok {
			call.Args = t.expandWords(call.Args)
		}
	}
}

func (t *aliasTable) expandWords(words []*syntax.Word) []*syntax.Word {
	active := make(map[string]bool)
	for i := 0; i < len(words); {
		var blank bool
		words, i, blank = t.expandAt(words, i, active)
		if !blank {
			break
		}
	}
	if len(words) == 0 {
		return words
	}
	// The first word can still name an alias when the expansion stopped at
	// an alias already being expanded, as with alias ls='ls -p'. Quoting it
	// keeps the interpreter from expanding it once more; a quoted command
	// name runs the same command.
	if name := words[0].Lit(); name != "" && !strings.ContainsAny(name, `\'"$*?[]{}~`) {
		if _, ok := t.defs[name]; ok {
			words = slices.Clone(words)
			words[0] = &syntax.Word{Parts: []syntax.WordPart{&syntax.SglQuoted{
				Left:  words[0].Pos(),
				Right: words[0].End(),
				Value: name,
			}}}
		}
	}
	return words
}

// expandAt expands the command word at words[i] and then the first word of
// its value, and so on. It returns the index just past the text the
// expansion produced and whether that text ends in a blank, so that the word
// after it is checked as well. active holds the aliases being expanded,
// which Bash does not expand again.
func (t *aliasTable) expandAt(words []*syntax.Word, i int, active map[string]bool) ([]*syntax.Word, int, bool) {
	name := words[i].Lit()
	def, ok := t.defs[name]
	if !ok || active[name] {
		return words, i + 1, false
	}
	active[name] = true
	defer delete(active, name)

	words = slices.Concat(words[:i], def.words, words[i+1:])
	end := i + len(def.words)
	blank := def.blank
	for j := i; j < end; {
		before := len(words)
		next, innerBlank := 0, false
		words, next, innerBlank = t.expandAt(words, j, active)
		end += len(words) - before
		if !innerBlank {
			break
		}
		if next >= end {
			// A blank at the very end of the value checks the word after it.
			blank = true
			break
		}
		j = next
	}
	return words, end, blank
}

func (d callDeps) rewriteAlias(ctx context.Context, argv []string) ([]string, bool) {
	if d.aliases != nil {
		d.aliases.define(argv[1:])
	}
	return nil, false
}

// rewriteUnalias mirrors removals and implements "unalias -a", which the
// interpreter would take as an alias named "-a", by naming every alias.
func (d callDeps) rewriteUnalias(ctx context.Context, argv []string) ([]string, bool) {
	if d.aliases == nil {
		return nil, false
	}
	names := argv[1:]
	if len(names) > 0 && names[0] == "--" {
		names = names[1:]
	}
	if len(argv) == 2 && argv[1] == "-a" {
		names = d.aliases.names()
		d.aliases.reset()
		return append([]string{argv[0]}, names...), true
	}
	d.aliases.remove(names)
	return append([]string{argv[0]}, names...), true
}
