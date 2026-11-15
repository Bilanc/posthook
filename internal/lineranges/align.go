package lineranges

// Alignment of two copies of a file by line fingerprint.
//
// The authorship map (store.FileMap) knows a file only as one fingerprint
// per line, and has to be lined up with a newer copy of the file every time
// posthook looks at it again: a line that is still there keeps its author.
// AlignLines above does this for text with a quadratic table, which is fine
// for the differing part of one edit and too slow for two copies of a large
// file that differ in several places. AlignHashes uses Myers' algorithm: its
// cost grows with the number of lines that differ, not with the size of the
// file. Where even that is too many (a lock file after a dependency bump),
// the two are cut at the unique lines they share and aligned piece by
// piece, so repeated lines between two unchanged unique ones are still
// paired.

// maxAlignEdits bounds the lines that may differ (inserted plus removed)
// between two stretches once their common start and end are set aside. The
// backtracking trace takes (edits+1)^2 four-byte entries, 16 MB at this
// bound. Two stretches that differ by more are cut at the unique lines they
// share, and each piece between two such lines is aligned on its own.
const maxAlignEdits = 2000

// maxAlignDepth bounds how often a stretch is cut again.
const maxAlignDepth = 12

// AlignHashes pairs the entries of a with the entries of b the way a line
// diff does: match[i] is the index in b of the entry a[i] is kept as, or -1
// when a[i] has no counterpart. Pairs never cross.
func AlignHashes(a, b []uint64) []int {
	match := make([]int, len(a))
	for i := range match {
		match[i] = -1
	}
	alignInto(match, a, b, 0, 0, 0)
	return match
}

// alignInto aligns a with b and writes the pairs into match. offA and offB
// are where a and b start in the sequences match is about.
func alignInto(match []int, a, b []uint64, offA, offB, depth int) {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		match[offA+prefix] = offB + prefix
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		match[offA+len(a)-1-suffix] = offB + len(b) - 1 - suffix
		suffix++
	}
	midA, midB := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	if len(midA) == 0 || len(midB) == 0 {
		return
	}
	offA, offB = offA+prefix, offB+prefix
	if pairs, ok := myersPairs(midA, midB, maxAlignEdits); ok {
		for _, p := range pairs {
			match[offA+p[0]] = offB + p[1]
		}
		return
	}
	// Too far apart for one edit script. The lines that occur once in each
	// and in the same order are certain pairs; what lies between two of
	// them is a smaller problem of the same kind. Without any such line, or
	// cut too often already, the stretch has no pairs.
	anchors := uniquePairs(midA, midB)
	if len(anchors) == 0 || depth == maxAlignDepth {
		return
	}
	fromA, fromB := 0, 0
	for _, anchor := range anchors {
		alignInto(match, midA[fromA:anchor[0]], midB[fromB:anchor[1]], offA+fromA, offB+fromB, depth+1)
		match[offA+anchor[0]] = offB + anchor[1]
		fromA, fromB = anchor[0]+1, anchor[1]+1
	}
	alignInto(match, midA[fromA:], midB[fromB:], offA+fromA, offB+fromB, depth+1)
}

// myersPairs returns the pairs of a shortest edit script between a and b, or
// ok false when the script is longer than maxEdits.
func myersPairs(a, b []uint64, maxEdits int) (pairs [][2]int, ok bool) {
	n, m := len(a), len(b)
	if maxEdits > n+m {
		maxEdits = n + m
	}
	// v[k+offset] is the furthest x reached on diagonal k (x - y == k).
	offset := maxEdits + 1
	v := make([]int32, 2*maxEdits+3)
	// trace[d] is v as it stood before step d, for diagonals -d..d.
	trace := make([][]int32, 0, 16)
	for d := 0; d <= maxEdits; d++ {
		saved := make([]int32, 2*d+1)
		copy(saved, v[offset-d:offset+d+1])
		trace = append(trace, saved)
		for k := -d; k <= d; k += 2 {
			var x int32
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - int32(k)
			for int(x) < n && int(y) < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if int(x) >= n && int(y) >= m {
				return backtrack(trace, d, n, m), true
			}
		}
	}
	return nil, false
}

// backtrack walks the trace from the end of both sequences to their start
// and collects the pairs on the way.
func backtrack(trace [][]int32, d, n, m int) [][2]int {
	var pairs [][2]int
	x, y := n, m
	for ; d > 0; d-- {
		saved := trace[d] // diagonal k is saved[k+d]
		k := x - y
		var prevK int
		if k == -d || (k != d && saved[k-1+d] < saved[k+1+d]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := int(saved[prevK+d])
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			pairs = append(pairs, [2]int{x, y})
		}
		x, y = prevX, prevY
	}
	for x > 0 && y > 0 {
		x--
		y--
		pairs = append(pairs, [2]int{x, y})
	}
	return pairs
}

// uniquePairs pairs the entries that occur exactly once in a and once in b,
// keeping those that are in the same order in both: the lines two copies of
// a file can be cut at when they differ by more than an edit script is
// worked out for.
func uniquePairs(a, b []uint64) [][2]int {
	const several = -2
	inB := make(map[uint64]int, len(b))
	for j, h := range b {
		if _, seen := inB[h]; seen {
			inB[h] = several
		} else {
			inB[h] = j
		}
	}
	countA := make(map[uint64]int, len(a))
	for _, h := range a {
		countA[h]++
	}
	var pairs [][2]int
	last := -1
	for i, h := range a {
		if countA[h] != 1 {
			continue
		}
		if j, found := inB[h]; found && j > last {
			pairs = append(pairs, [2]int{i, j})
			last = j
		}
	}
	return pairs
}
