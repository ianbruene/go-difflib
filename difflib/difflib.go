// Package difflib is a partial port of Python difflib module.
//
// It provides tools to compare sequences of strings and generate textual diffs.
//
// The following class and functions have been ported:
//
// - SequenceMatcher
//
// - unified_diff
//
// - context_diff
//
// Getting unified diffs was the main goal of the port. Keep in mind this code
// is mostly suitable to output text differences in a human friendly way, there
// are no guarantees generated diffs are consumable by patch(1).
package difflib

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func calculateRatio(matches, length int) float64 {
	if length > 0 {
		return 2.0 * float64(matches) / float64(length)
	}
	return 1.0
}

func listifyString(str string) (lst []string) {
	lst = make([]string, len(str))
	for i, c := range str {
		lst[i] = string(c)
	}
	return lst
}

type Match struct {
	A    int
	B    int
	Size int
}

type OpCode struct {
	Tag byte
	I1  int
	I2  int
	J1  int
	J2  int
}

// SequenceMatcher compares sequence of strings. The basic
// algorithm predates, and is a little fancier than, an algorithm
// published in the late 1980's by Ratcliff and Obershelp under the
// hyperbolic name "gestalt pattern matching".  The basic idea is to find
// the longest contiguous matching subsequence that contains no "junk"
// elements (R-O doesn't address junk).  The same idea is then applied
// recursively to the pieces of the sequences to the left and to the right
// of the matching subsequence.  This does not yield minimal edit
// sequences, but does tend to yield matches that "look right" to people.
//
// SequenceMatcher tries to compute a "human-friendly diff" between two
// sequences.  Unlike e.g. UNIX(tm) diff, the fundamental notion is the
// longest *contiguous* & junk-free matching subsequence.  That's what
// catches peoples' eyes.  The Windows(tm) windiff has another interesting
// notion, pairing up elements that appear uniquely in each sequence.
// That, and the method here, appear to yield more intuitive difference
// reports than does diff.  This method appears to be the least vulnerable
// to synching up on blocks of "junk lines", though (like blank lines in
// ordinary text files, or maybe "<P>" lines in HTML files).  That may be
// because this is the only method of the 3 that has a *concept* of
// "junk" <wink>.
//
// Timing:  Basic R-O is cubic time worst case and quadratic time expected
// case.  SequenceMatcher is quadratic time for the worst case and has
// expected-case behavior dependent in a complicated way on how many
// elements the sequences have in common; best case time is linear.
type SequenceMatcher struct {
	a              []string
	b              []string
	b2j            map[string][]int
	IsJunk         func(string) bool
	autoJunk       bool
	bJunk          map[string]bool
	matchingBlocks []Match
	fullBCount     map[string]int
	bPopular       map[string]bool
	opCodes        []OpCode
}

func NewMatcher(a, b []string) *SequenceMatcher {
	m := SequenceMatcher{autoJunk: true}
	m.SetSeqs(a, b)
	return &m
}

func NewMatcherWithJunk(a, b []string, autoJunk bool,
	isJunk func(string) bool) *SequenceMatcher {

	m := SequenceMatcher{IsJunk: isJunk, autoJunk: autoJunk}
	m.SetSeqs(a, b)
	return &m
}

// Set two sequences to be compared.
func (m *SequenceMatcher) SetSeqs(a, b []string) {
	m.SetSeq1(a)
	m.SetSeq2(b)
}

// Set the first sequence to be compared. The second sequence to be compared is
// not changed.
//
// SequenceMatcher computes and caches detailed information about the second
// sequence, so if you want to compare one sequence S against many sequences,
// use .SetSeq2(s) once and call .SetSeq1(x) repeatedly for each of the other
// sequences.
//
// See also SetSeqs() and SetSeq2().
func (m *SequenceMatcher) SetSeq1(a []string) {
	if &a == &m.a {
		return
	}
	m.a = a
	m.matchingBlocks = nil
	m.opCodes = nil
}

// Set the second sequence to be compared. The first sequence to be compared is
// not changed.
func (m *SequenceMatcher) SetSeq2(b []string) {
	if &b == &m.b {
		return
	}
	m.b = b
	m.matchingBlocks = nil
	m.opCodes = nil
	m.fullBCount = nil
	m.chainB()
}

func (m *SequenceMatcher) chainB() {
	// Populate line -> index mapping
	b2j := map[string][]int{}
	junk := map[string]bool{}
	popular := map[string]bool{}
	ntest := len(m.b)
	if m.autoJunk && ntest >= 200 {
		ntest = ntest/100 + 1
	}
	for i, s := range m.b {
		if !junk[s] {
			if m.IsJunk != nil && m.IsJunk(s) {
				junk[s] = true
			} else if !popular[s] {
				ids := append(b2j[s], i)
				if len(ids) <= ntest {
					b2j[s] = ids
				} else {
					delete(b2j, s)
					popular[s] = true
				}
			}
		}
	}

	m.b2j = b2j
	m.bJunk = junk
	m.bPopular = popular
}

func (m *SequenceMatcher) isBJunk(s string) bool {
	_, ok := m.bJunk[s]
	return ok
}

// Find longest matching block in a[alo:ahi] and b[blo:bhi].
//
// If IsJunk is not defined:
//
// Return (i,j,k) such that a[i:i+k] is equal to b[j:j+k], where
//
//	alo <= i <= i+k <= ahi
//	blo <= j <= j+k <= bhi
//
// and for all (i',j',k') meeting those conditions,
//
//	k >= k'
//	i <= i'
//	and if i == i', j <= j'
//
// In other words, of all maximal matching blocks, return one that
// starts earliest in a, and of all those maximal matching blocks that
// start earliest in a, return the one that starts earliest in b.
//
// If IsJunk is defined, first the longest matching block is
// determined as above, but with the additional restriction that no
// junk element appears in the block.  Then that block is extended as
// far as possible by matching (only) junk elements on both sides.  So
// the resulting block never matches on junk except as identical junk
// happens to be adjacent to an "interesting" match.
//
// If no blocks match, return (alo, blo, 0).
func (m *SequenceMatcher) findLongestMatch(alo, ahi, blo, bhi int) Match {
	// CAUTION:  stripping common prefix or suffix would be incorrect.
	// E.g.,
	//    ab
	//    acab
	// Longest matching block is "ab", but if common prefix is
	// stripped, it's "a" (tied with "b").  UNIX(tm) diff does so
	// strip, so ends up claiming that ab is changed to acab by
	// inserting "ca" in the middle.  That's minimal but unintuitive:
	// "it's obvious" that someone inserted "ac" at the front.
	// Windiff ends up at the same place as diff, but by pairing up
	// the unique 'b's and then matching the first two 'a's.
	besti, bestj, bestsize := alo, blo, 0

	// find longest junk-free match
	// during an iteration of the loop, j2len[j] = length of longest
	// junk-free match ending with a[i-1] and b[j]
	N := bhi - blo
	j2len := make([]int, N)
	newj2len := make([]int, N)
	var indices []int
	for i := alo; i != ahi; i++ {
		// look at all instances of a[i] in b; note that because
		// b2j has no junk keys, the loop is skipped if a[i] is junk
		newindices := m.b2j[m.a[i]]
		for _, j := range newindices {
			// a[i] matches b[j]
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := 1
			if j > blo {
				k = j2len[j-1-blo] + 1
			}
			newj2len[j-blo] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		// j2len = newj2len, clear and reuse j2len as newj2len
		for _, j := range indices {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			j2len[j-blo] = 0
		}
		indices = newindices
		j2len, newj2len = newj2len, j2len
	}

	// Extend the best by non-junk elements on each end.  In particular,
	// "popular" non-junk elements aren't in b2j, which greatly speeds
	// the inner loop above, but also means "the best" match so far
	// doesn't contain any junk *or* popular non-junk elements.
	for besti > alo && bestj > blo && !m.isBJunk(m.b[bestj-1]) &&
		m.a[besti-1] == m.b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi &&
		!m.isBJunk(m.b[bestj+bestsize]) &&
		m.a[besti+bestsize] == m.b[bestj+bestsize] {
		bestsize += 1
	}

	// Now that we have a wholly interesting match (albeit possibly
	// empty!), we may as well suck up the matching junk on each
	// side of it too.  Can't think of a good reason not to, and it
	// saves post-processing the (possibly considerable) expense of
	// figuring out what to do with it.  In the case of an empty
	// interesting match, this is clearly the right thing to do,
	// because no other kind of match is possible in the regions.
	for besti > alo && bestj > blo && m.isBJunk(m.b[bestj-1]) &&
		m.a[besti-1] == m.b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi &&
		m.isBJunk(m.b[bestj+bestsize]) &&
		m.a[besti+bestsize] == m.b[bestj+bestsize] {
		bestsize += 1
	}

	return Match{A: besti, B: bestj, Size: bestsize}
}

// Return list of triples describing matching subsequences.
//
// Each triple is of the form (i, j, n), and means that
// a[i:i+n] == b[j:j+n].  The triples are monotonically increasing in
// i and in j. It's also guaranteed that if (i, j, n) and (i', j', n') are
// adjacent triples in the list, and the second is not the last triple in the
// list, then i+n != i' or j+n != j'. IOW, adjacent triples never describe
// adjacent equal blocks.
//
// The last triple is a dummy, (len(a), len(b), 0), and is the only
// triple with n==0.
func (m *SequenceMatcher) GetMatchingBlocks() []Match {
	if m.matchingBlocks != nil {
		return m.matchingBlocks
	}

	var matchBlocks func(alo, ahi, blo, bhi int, matched []Match) []Match
	matchBlocks = func(alo, ahi, blo, bhi int, matched []Match) []Match {
		match := m.findLongestMatch(alo, ahi, blo, bhi)
		i, j, k := match.A, match.B, match.Size
		if match.Size > 0 {
			if alo < i && blo < j {
				matched = matchBlocks(alo, i, blo, j, matched)
			}
			matched = append(matched, match)
			if i+k < ahi && j+k < bhi {
				matched = matchBlocks(i+k, ahi, j+k, bhi, matched)
			}
		}
		return matched
	}
	matched := matchBlocks(0, len(m.a), 0, len(m.b), nil)

	// It's possible that we have adjacent equal blocks in the
	// matching_blocks list now.
	nonAdjacent := []Match{}
	i1, j1, k1 := 0, 0, 0
	for _, b := range matched {
		// Is this block adjacent to i1, j1, k1?
		i2, j2, k2 := b.A, b.B, b.Size
		if i1+k1 == i2 && j1+k1 == j2 {
			// Yes, so collapse them -- this just increases the length of
			// the first block by the length of the second, and the first
			// block so lengthened remains the block to compare against.
			k1 += k2
		} else {
			// Not adjacent.  Remember the first block (k1==0 means it's
			// the dummy we started with), and make the second block the
			// new block to compare against.
			if k1 > 0 {
				nonAdjacent = append(nonAdjacent, Match{i1, j1, k1})
			}
			i1, j1, k1 = i2, j2, k2
		}
	}
	if k1 > 0 {
		nonAdjacent = append(nonAdjacent, Match{i1, j1, k1})
	}

	nonAdjacent = append(nonAdjacent, Match{len(m.a), len(m.b), 0})
	m.matchingBlocks = nonAdjacent
	return m.matchingBlocks
}

// Return list of 5-tuples describing how to turn a into b.
//
// Each tuple is of the form (tag, i1, i2, j1, j2).  The first tuple
// has i1 == j1 == 0, and remaining tuples have i1 == the i2 from the
// tuple preceding it, and likewise for j1 == the previous j2.
//
// The tags are characters, with these meanings:
//
// 'r' (replace):  a[i1:i2] should be replaced by b[j1:j2]
//
// 'd' (delete):   a[i1:i2] should be deleted, j1==j2 in this case.
//
// 'i' (insert):   b[j1:j2] should be inserted at a[i1:i1], i1==i2 in this case.
//
// 'e' (equal):    a[i1:i2] == b[j1:j2]
func (m *SequenceMatcher) GetOpCodes() []OpCode {
	if m.opCodes != nil {
		return m.opCodes
	}
	i, j := 0, 0
	matching := m.GetMatchingBlocks()
	opCodes := make([]OpCode, 0, len(matching))
	for _, m := range matching {
		//  invariant:  we've pumped out correct diffs to change
		//  a[:i] into b[:j], and the next matching block is
		//  a[ai:ai+size] == b[bj:bj+size]. So we need to pump
		//  out a diff to change a[i:ai] into b[j:bj], pump out
		//  the matching block, and move (i,j) beyond the match
		ai, bj, size := m.A, m.B, m.Size
		tag := byte(0)
		if i < ai && j < bj {
			tag = 'r'
		} else if i < ai {
			tag = 'd'
		} else if j < bj {
			tag = 'i'
		}
		if tag > 0 {
			opCodes = append(opCodes, OpCode{tag, i, ai, j, bj})
		}
		i, j = ai+size, bj+size
		// the list of matching blocks is terminated by a
		// sentinel with size 0
		if size > 0 {
			opCodes = append(opCodes, OpCode{'e', ai, i, bj, j})
		}
	}
	m.opCodes = opCodes
	return m.opCodes
}

// Isolate change clusters by eliminating ranges with no changes.
//
// Return a generator of groups with up to n lines of context.
// Each group is in the same format as returned by GetOpCodes().
func (m *SequenceMatcher) GetGroupedOpCodes(n int) [][]OpCode {
	if n < 0 {
		n = 3
	}
	codes := m.GetOpCodes()
	if len(codes) == 0 {
		codes = []OpCode{OpCode{'e', 0, 1, 0, 1}}
	}
	// Fixup leading and trailing groups if they show no changes.
	if codes[0].Tag == 'e' {
		c := codes[0]
		i1, i2, j1, j2 := c.I1, c.I2, c.J1, c.J2
		codes[0] = OpCode{c.Tag, max(i1, i2-n), i2, max(j1, j2-n), j2}
	}
	if codes[len(codes)-1].Tag == 'e' {
		c := codes[len(codes)-1]
		i1, i2, j1, j2 := c.I1, c.I2, c.J1, c.J2
		codes[len(codes)-1] = OpCode{c.Tag, i1, min(i2, i1+n), j1, min(j2, j1+n)}
	}
	nn := n + n
	groups := [][]OpCode{}
	group := []OpCode{}
	for _, c := range codes {
		i1, i2, j1, j2 := c.I1, c.I2, c.J1, c.J2
		// End the current group and start a new one whenever
		// there is a large range with no changes.
		if c.Tag == 'e' && i2-i1 > nn {
			group = append(group, OpCode{c.Tag, i1, min(i2, i1+n),
				j1, min(j2, j1+n)})
			groups = append(groups, group)
			group = []OpCode{}
			i1, j1 = max(i1, i2-n), max(j1, j2-n)
		}
		group = append(group, OpCode{c.Tag, i1, i2, j1, j2})
	}
	if len(group) > 0 && !(len(group) == 1 && group[0].Tag == 'e') {
		groups = append(groups, group)
	}
	return groups
}

// Return a measure of the sequences' similarity (float in [0,1]).
//
// Where T is the total number of elements in both sequences, and
// M is the number of matches, this is 2.0*M / T.
// Note that this is 1 if the sequences are identical, and 0 if
// they have nothing in common.
//
// .Ratio() is expensive to compute if you haven't already computed
// .GetMatchingBlocks() or .GetOpCodes(), in which case you may
// want to try .QuickRatio() or .RealQuickRation() first to get an
// upper bound.
func (m *SequenceMatcher) Ratio() float64 {
	matches := 0
	for _, m := range m.GetMatchingBlocks() {
		matches += m.Size
	}
	return calculateRatio(matches, len(m.a)+len(m.b))
}

// Return an upper bound on ratio() relatively quickly.
//
// This isn't defined beyond that it is an upper bound on .Ratio(), and
// is faster to compute.
func (m *SequenceMatcher) QuickRatio() float64 {
	// viewing a and b as multisets, set matches to the cardinality
	// of their intersection; this counts the number of matches
	// without regard to order, so is clearly an upper bound
	if m.fullBCount == nil {
		m.fullBCount = map[string]int{}
		for _, s := range m.b {
			m.fullBCount[s] = m.fullBCount[s] + 1
		}
	}

	// avail[x] is the number of times x appears in 'b' less the
	// number of times we've seen it in 'a' so far ... kinda
	avail := map[string]int{}
	matches := 0
	for _, s := range m.a {
		n, ok := avail[s]
		if !ok {
			n = m.fullBCount[s]
		}
		avail[s] = n - 1
		if n > 0 {
			matches += 1
		}
	}
	return calculateRatio(matches, len(m.a)+len(m.b))
}

// Return an upper bound on ratio() very quickly.
//
// This isn't defined beyond that it is an upper bound on .Ratio(), and
// is faster to compute than either .Ratio() or .QuickRatio().
func (m *SequenceMatcher) RealQuickRatio() float64 {
	la, lb := len(m.a), len(m.b)
	return calculateRatio(min(la, lb), la+lb)
}

func count_leading(line string, ch byte) (count int) {
	// Return number of `ch` characters at the start of `line`.
	count = 0
	n := len(line)
	for (count < n) && (line[count] == ch) {
		count++
	}
	return count
}

type DiffLine struct {
	Tag  byte
	Line string
}

func NewDiffLine(tag byte, line string) (l DiffLine) {
	l = DiffLine{}
	l.Tag = tag
	l.Line = line
	return l
}

type Differ struct {
	Linejunk func(string) bool
	Charjunk func(string) bool
}

func NewDiffer() *Differ {
	return &Differ{}
}

func (d *Differ) Compare(a []string, b []string) (diffs []string, err error) {
	// Compare two sequences of lines; generate the resulting delta.

	// Each sequence must contain individual single-line strings ending with
	// newlines. Such sequences can be obtained from the `readlines()` method
	// of file-like objects.  The delta generated also consists of newline-
	// terminated strings, ready to be printed as-is via the writeline()
	// method of a file-like object.
	diffs = []string{}
	cruncher := NewMatcherWithJunk(a, b, true, d.Linejunk)
	opcodes := cruncher.GetOpCodes()
	for _, current := range opcodes {
		alo := current.I1
		ahi := current.I2
		blo := current.J1
		bhi := current.J2
		var g []string
		if current.Tag == 'r' {
			g, _ = d.FancyReplace(a, alo, ahi, b, blo, bhi)
		} else if current.Tag == 'd' {
			g = d.Dump("-", a, alo, ahi)
		} else if current.Tag == 'i' {
			g = d.Dump("+", b, blo, bhi)
		} else if current.Tag == 'e' {
			g = d.Dump(" ", a, alo, ahi)
		} else {
			return nil, errors.New(fmt.Sprintf("unknown tag %q", current.Tag))
		}
		diffs = append(diffs, g...)
	}
	return diffs, nil
}

func (d *Differ) StructuredDump(tag byte, x []string, low int, high int) (out []DiffLine) {
	size := high - low
	out = make([]DiffLine, size)
	for i := 0; i < size; i++ {
		out[i] = NewDiffLine(tag, x[i+low])
	}
	return out
}

func (d *Differ) Dump(tag string, x []string, low int, high int) (out []string) {
	// Generate comparison results for a same-tagged range.
	sout := d.StructuredDump(tag[0], x, low, high)
	out = make([]string, len(sout))
	var bld strings.Builder
	bld.Grow(1024)
	for i, line := range sout {
		bld.Reset()
		bld.WriteByte(line.Tag)
		bld.WriteString(" ")
		bld.WriteString(line.Line)
		out[i] = bld.String()
	}
	return out
}

func (d *Differ) PlainReplace(a []string, alo int, ahi int, b []string, blo int, bhi int) (out []string, err error) {
	if !(alo < ahi) || !(blo < bhi) { // assertion
		return nil, errors.New("low greater than or equal to high")
	}
	// dump the shorter block first -- reduces the burden on short-term
	// memory if the blocks are of very different sizes
	if bhi-blo < ahi-alo {
		out = d.Dump("+", b, blo, bhi)
		out = append(out, d.Dump("-", a, alo, ahi)...)
	} else {
		out = d.Dump("-", a, alo, ahi)
		out = append(out, d.Dump("+", b, blo, bhi)...)
	}
	return out, nil
}

func (d *Differ) FancyReplace(a []string, alo int, ahi int, b []string, blo int, bhi int) (out []string, err error) {
	// When replacing one block of lines with another, search the blocks
	// for *similar* lines; the best-matching pair (if any) is used as a
	// synch point, and intraline difference marking is done on the
	// similar pair. Lots of work, but often worth it.

	// don't synch up unless the lines have a similarity score of at
	// least cutoff; best_ratio tracks the best score seen so far
	best_ratio := 0.74
	cutoff := 0.75
	cruncher := NewMatcherWithJunk(a, b, true, d.Charjunk)
	eqi := -1 // 1st indices of equal lines (if any)
	eqj := -1
	out = []string{}

	// search for the pair that matches best without being identical
	// (identical lines must be junk lines, & we don't want to synch up
	// on junk -- unless we have to)
	var best_i, best_j int
	for j := blo; j < bhi; j++ {
		bj := b[j]
		cruncher.SetSeq2(listifyString(bj))
		for i := alo; i < ahi; i++ {
			ai := a[i]
			if ai == bj {
				if eqi == -1 {
					eqi = i
					eqj = j
				}
				continue
			}
			cruncher.SetSeq1(listifyString(ai))
			// computing similarity is expensive, so use the quick
			// upper bounds first -- have seen this speed up messy
			// compares by a factor of 3.
			// note that ratio() is only expensive to compute the first
			// time it's called on a sequence pair; the expensive part
			// of the computation is cached by cruncher
			if cruncher.RealQuickRatio() > best_ratio &&
				cruncher.QuickRatio() > best_ratio &&
				cruncher.Ratio() > best_ratio {
				best_ratio = cruncher.Ratio()
				best_i = i
				best_j = j
			}
		}
	}
	if best_ratio < cutoff {
		// no non-identical "pretty close" pair
		if eqi == -1 {
			// no identical pair either -- treat it as a straight replace
			out, _ = d.PlainReplace(a, alo, ahi, b, blo, bhi)
			return out, nil
		}
		// no close pair, but an identical pair -- synch up on that
		best_i = eqi
		best_j = eqj
		best_ratio = 1.0
	} else {
		// there's a close pair, so forget the identical pair (if any)
		eqi = -1
	}
	// a[best_i] very similar to b[best_j]; eqi is None iff they're not
	// identical

	// pump out diffs from before the synch point
	out = append(out, d.fancyHelper(a, alo, best_i, b, blo, best_j)...)

	// do intraline marking on the synch pair
	aelt, belt := a[best_i], b[best_j]
	if eqi == -1 {
		// pump out a '-', '?', '+', '?' quad for the synched lines
		var atags, btags string
		cruncher.SetSeqs(listifyString(aelt), listifyString(belt))
		opcodes := cruncher.GetOpCodes()
		for _, current := range opcodes {
			ai1 := current.I1
			ai2 := current.I2
			bj1 := current.J1
			bj2 := current.J2
			la, lb := ai2-ai1, bj2-bj1
			if current.Tag == 'r' {
				atags += strings.Repeat("^", la)
				btags += strings.Repeat("^", lb)
			} else if current.Tag == 'd' {
				atags += strings.Repeat("-", la)
			} else if current.Tag == 'i' {
				btags += strings.Repeat("+", lb)
			} else if current.Tag == 'e' {
				atags += strings.Repeat(" ", la)
				btags += strings.Repeat(" ", lb)
			} else {
				return nil, errors.New(fmt.Sprintf("unknown tag %q",
					current.Tag))
			}
		}
		out = append(out, d.QFormat(aelt, belt, atags, btags)...)
	} else {
		// the synch pair is identical
		out = append(out, "  "+aelt)
	}
	// pump out diffs from after the synch point
	out = append(out, d.fancyHelper(a, best_i+1, ahi, b, best_j+1, bhi)...)
	return out, nil
}

func (d *Differ) fancyHelper(a []string, alo int, ahi int, b []string, blo int, bhi int) (out []string) {
	if alo < ahi {
		if blo < bhi {
			out, _ = d.FancyReplace(a, alo, ahi, b, blo, bhi)
		} else {
			out = d.Dump("-", a, alo, ahi)
		}
	} else if blo < bhi {
		out = d.Dump("+", b, blo, bhi)
	} else {
		out = []string{}
	}
	return out
}

func (d *Differ) QFormat(aline string, bline string, atags string, btags string) (out []string) {
	// Format "?" output and deal with leading tabs.

	// Can hurt, but will probably help most of the time.
	common := min(count_leading(aline, '\t'), count_leading(bline, '\t'))
	common = min(common, count_leading(atags[:common], ' '))
	common = min(common, count_leading(btags[:common], ' '))
	atags = strings.TrimRightFunc(atags[common:], unicode.IsSpace)
	btags = strings.TrimRightFunc(btags[common:], unicode.IsSpace)

	out = []string{"- " + aline}
	if len(atags) > 0 {
		out = append(out, fmt.Sprintf("? %s%s\n",
			strings.Repeat("\t", common), atags))
	}
	out = append(out, "+ "+bline)
	if len(btags) > 0 {
		out = append(out, fmt.Sprintf("? %s%s\n",
			strings.Repeat("\t", common), btags))
	}
	return out
}

// Convert range to the "ed" format
func formatRangeUnified(start, stop int) string {
	// Per the diff spec at http://www.unix.org/single_unix_specification/
	beginning := start + 1 // lines start numbering with one
	length := stop - start
	if length == 1 {
		return fmt.Sprintf("%d", beginning)
	}
	if length == 0 {
		beginning -= 1 // empty ranges begin at line just before the range
	}
	return fmt.Sprintf("%d,%d", beginning, length)
}

// Unified diff parameters
type LineDiffParams struct {
	A          []string          // First sequence lines
	FromFile   string            // First file name
	FromDate   string            // First file time
	B          []string          // Second sequence lines
	ToFile     string            // Second file name
	ToDate     string            // Second file time
	Eol        string            // Headers end of line, defaults to LF
	Context    int               // Number of context lines
	AutoJunk   bool              // If true, use autojunking
	IsJunkLine func(string) bool // How to spot junk lines
}

// Compare two sequences of lines; generate the delta as a unified diff.
//
// Unified diffs are a compact way of showing line changes and a few
// lines of context.  The number of context lines is set by 'n' which
// defaults to three.
//
// By default, the diff control lines (those with ---, +++, or @@) are
// created with a trailing newline.  This is helpful so that inputs
// created from file.readlines() result in diffs that are suitable for
// file.writelines() since both the inputs and outputs have trailing
// newlines.
//
// For inputs that do not have trailing newlines, set the lineterm
// argument to "" so that the output will be uniformly newline free.
//
// The unidiff format normally has a header for filenames and modification
// times.  Any or all of these may be specified using strings for
// 'fromfile', 'tofile', 'fromfiledate', and 'tofiledate'.
// The modification times are normally expressed in the ISO 8601 format.
func WriteUnifiedDiff(writer io.Writer, diff LineDiffParams) error {
	//buf := bufio.NewWriter(writer)
	//defer buf.Flush()
	var bld strings.Builder
	bld.Reset()
	wf := func(format string, args ...interface{}) error {
		_, err := fmt.Fprintf(&bld, format, args...)
		return err
	}
	ws := func(s string) error {
		_, err := bld.WriteString(s)
		return err
	}

	if len(diff.Eol) == 0 {
		diff.Eol = "\n"
	}

	started := false
	m := NewMatcher(diff.A, diff.B)
	if diff.AutoJunk || diff.IsJunkLine != nil {
		m = NewMatcherWithJunk(diff.A, diff.B, diff.AutoJunk, diff.IsJunkLine)
	}
	for _, g := range m.GetGroupedOpCodes(diff.Context) {
		if !started {
			started = true
			fromDate := ""
			if len(diff.FromDate) > 0 {
				fromDate = "\t" + diff.FromDate
			}
			toDate := ""
			if len(diff.ToDate) > 0 {
				toDate = "\t" + diff.ToDate
			}
			if diff.FromFile != "" || diff.ToFile != "" {
				err := wf("--- %s%s%s", diff.FromFile, fromDate, diff.Eol)
				if err != nil {
					return err
				}
				err = wf("+++ %s%s%s", diff.ToFile, toDate, diff.Eol)
				if err != nil {
					return err
				}
			}
		}
		first, last := g[0], g[len(g)-1]
		range1 := formatRangeUnified(first.I1, last.I2)
		range2 := formatRangeUnified(first.J1, last.J2)
		if err := wf("@@ -%s +%s @@%s", range1, range2, diff.Eol); err != nil {
			return err
		}
		for _, c := range g {
			i1, i2, j1, j2 := c.I1, c.I2, c.J1, c.J2
			if c.Tag == 'e' {
				for _, line := range diff.A[i1:i2] {
					if err := ws(" " + line); err != nil {
						return err
					}
				}
				continue
			}
			if c.Tag == 'r' || c.Tag == 'd' {
				for _, line := range diff.A[i1:i2] {
					if err := ws("-" + line); err != nil {
						return err
					}
				}
			}
			if c.Tag == 'r' || c.Tag == 'i' {
				for _, line := range diff.B[j1:j2] {
					if err := ws("+" + line); err != nil {
						return err
					}
				}
			}
		}
	}
	buf := bufio.NewWriter(writer)
	buf.WriteString(bld.String())
	buf.Flush()
	return nil
}

// Like WriteUnifiedDiff but returns the diff a string.
func GetUnifiedDiffString(diff LineDiffParams) (string, error) {
	w := &bytes.Buffer{}
	err := WriteUnifiedDiff(w, diff)
	return string(w.Bytes()), err
}

// Convert range to the "ed" format.
func formatRangeContext(start, stop int) string {
	// Per the diff spec at http://www.unix.org/single_unix_specification/
	beginning := start + 1 // lines start numbering with one
	length := stop - start
	if length == 0 {
		beginning -= 1 // empty ranges begin at line just before the range
	}
	if length <= 1 {
		return fmt.Sprintf("%d", beginning)
	}
	return fmt.Sprintf("%d,%d", beginning, beginning+length-1)
}

// For backward compatibility. Ugh.
type ContextDiff = LineDiffParams
type UnifiedDiff = LineDiffParams

// Compare two sequences of lines; generate the delta as a context diff.
//
// Context diffs are a compact way of showing line changes and a few
// lines of context. The number of context lines is set by diff.Context
// which defaults to three.
//
// By default, the diff control lines (those with *** or ---) are
// created with a trailing newline.
//
// For inputs that do not have trailing newlines, set the diff.Eol
// argument to "" so that the output will be uniformly newline free.
//
// The context diff format normally has a header for filenames and
// modification times.  Any or all of these may be specified using
// strings for diff.FromFile, diff.ToFile, diff.FromDate, diff.ToDate.
// The modification times are normally expressed in the ISO 8601 format.
// If not specified, the strings default to blanks.
func WriteContextDiff(writer io.Writer, diff LineDiffParams) error {
	buf := bufio.NewWriter(writer)
	defer buf.Flush()
	var diffErr error
	wf := func(format string, args ...interface{}) {
		_, err := buf.WriteString(fmt.Sprintf(format, args...))
		if diffErr == nil && err != nil {
			diffErr = err
		}
	}
	ws := func(s string) {
		_, err := buf.WriteString(s)
		if diffErr == nil && err != nil {
			diffErr = err
		}
	}

	if len(diff.Eol) == 0 {
		diff.Eol = "\n"
	}

	prefix := map[byte]string{
		'i': "+ ",
		'd': "- ",
		'r': "! ",
		'e': "  ",
	}

	started := false
	m := NewMatcher(diff.A, diff.B)
	if diff.AutoJunk || diff.IsJunkLine != nil {
		m = NewMatcherWithJunk(diff.A, diff.B, diff.AutoJunk, diff.IsJunkLine)
	}
	for _, g := range m.GetGroupedOpCodes(diff.Context) {
		if !started {
			started = true
			fromDate := ""
			if len(diff.FromDate) > 0 {
				fromDate = "\t" + diff.FromDate
			}
			toDate := ""
			if len(diff.ToDate) > 0 {
				toDate = "\t" + diff.ToDate
			}
			if diff.FromFile != "" || diff.ToFile != "" {
				wf("*** %s%s%s", diff.FromFile, fromDate, diff.Eol)
				wf("--- %s%s%s", diff.ToFile, toDate, diff.Eol)
			}
		}

		first, last := g[0], g[len(g)-1]
		ws("***************" + diff.Eol)

		range1 := formatRangeContext(first.I1, last.I2)
		wf("*** %s ****%s", range1, diff.Eol)
		for _, c := range g {
			if c.Tag == 'r' || c.Tag == 'd' {
				for _, cc := range g {
					if cc.Tag == 'i' {
						continue
					}
					for _, line := range diff.A[cc.I1:cc.I2] {
						ws(prefix[cc.Tag] + line)
					}
				}
				break
			}
		}

		range2 := formatRangeContext(first.J1, last.J2)
		wf("--- %s ----%s", range2, diff.Eol)
		for _, c := range g {
			if c.Tag == 'r' || c.Tag == 'i' {
				for _, cc := range g {
					if cc.Tag == 'd' {
						continue
					}
					for _, line := range diff.B[cc.J1:cc.J2] {
						ws(prefix[cc.Tag] + line)
					}
				}
				break
			}
		}
	}
	return diffErr
}

// Like WriteContextDiff but returns the diff as a string.
func GetContextDiffString(diff LineDiffParams) (string, error) {
	w := &bytes.Buffer{}
	err := WriteContextDiff(w, diff)
	return string(w.Bytes()), err
}

// Split a string on "\n" while preserving them. The output can be used
// as input for LineDiffParams.
func SplitLines(s string) []string {
	lines := strings.SplitAfter(s, "\n")
	lines[len(lines)-1] += "\n"
	return lines
}
