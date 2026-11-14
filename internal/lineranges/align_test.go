package lineranges

import (
	"math/rand"
	"reflect"
	"testing"
)

func hashesOf(s string) []uint64 {
	out := make([]uint64, len(s))
	for i, r := range []byte(s) {
		out[i] = uint64(r)
	}
	return out
}

func TestAlignHashes(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want []int
	}{
		{"identical", "abc", "abc", []int{0, 1, 2}},
		{"both empty", "", "", []int{}},
		{"all removed", "abc", "", []int{-1, -1, -1}},
		{"all new", "", "abc", []int{}},
		{"insert above", "abc", "xyabc", []int{2, 3, 4}},
		{"insert in the middle", "abcd", "abXYcd", []int{0, 1, 4, 5}},
		{"one line changed", "abcd", "abXd", []int{0, 1, -1, 3}},
		{"line removed", "abcd", "abd", []int{0, 1, -1, 2}},
		{"changes at both ends", "abcdef", "XbcdeY", []int{-1, 1, 2, 3, 4, -1}},
		{"nothing shared", "abc", "xyz", []int{-1, -1, -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AlignHashes(hashesOf(c.a), hashesOf(c.b))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("AlignHashes(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// Every pair must join equal entries, pairs must not cross, and the number
// of pairs must be the length of the longest common subsequence.
func TestAlignHashesAgainstTable(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 300; round++ {
		a := randomSequence(rng, rng.Intn(40), 6)
		b := randomSequence(rng, rng.Intn(40), 6)
		match := AlignHashes(a, b)
		last, pairs := -1, 0
		for i, j := range match {
			if j < 0 {
				continue
			}
			if a[i] != b[j] {
				t.Fatalf("round %d: paired unequal entries a[%d]=%d b[%d]=%d", round, i, a[i], j, b[j])
			}
			if j <= last {
				t.Fatalf("round %d: pairs cross at a[%d] -> b[%d]", round, i, j)
			}
			last = j
			pairs++
		}
		if want := lcsLength(a, b); pairs != want {
			t.Fatalf("round %d: %d pairs, longest common subsequence is %d (a=%v b=%v)", round, pairs, want, a, b)
		}
	}
}

func TestAlignHashesPastTheEditBound(t *testing.T) {
	// Two long sequences that share only two entries, far more edits apart
	// than an edit script is worked out for. The two that read like no
	// other are still paired.
	n := maxAlignEdits + 50
	a := make([]uint64, 0, n+2)
	b := make([]uint64, 0, n+2)
	for i := 0; i < n; i++ {
		a = append(a, uint64(1_000_000+i))
		b = append(b, uint64(2_000_000+i))
		if i == 10 {
			a = append(a, 7)
			b = append(b, 7)
		}
		if i == n-10 {
			a = append(a, 9)
			b = append(b, 9)
		}
	}
	match := AlignHashes(a, b)
	pairs := 0
	for i, j := range match {
		if j >= 0 {
			pairs++
			if a[i] != b[j] {
				t.Fatalf("paired unequal entries")
			}
		}
	}
	if pairs != 2 {
		t.Errorf("%d pairs, want the 2 unique shared entries", pairs)
	}
}

// A lock file after a dependency bump: thousands of lines differ, far past
// the edit bound, and most of the unchanged lines are ones that repeat (a
// brace, a flag). Every unchanged line must still be paired, the repeated
// ones too: a line with no pair reads as new.
func TestAlignHashesKeepsRepeatedLinesPastTheEditBound(t *testing.T) {
	const packages = 1500
	const repeated = 5 // stands for `},` and the like
	var a, b []uint64
	unchanged := 0
	for p := 0; p < packages; p++ {
		name := uint64(10_000_000 + p) // unique, unchanged
		a, b = append(a, name), append(b, name)
		unchanged++
		// A version line that changes in every package.
		a, b = append(a, uint64(20_000_000+p)), append(b, uint64(30_000_000+p))
		// An integrity line that changes too.
		a, b = append(a, uint64(40_000_000+p)), append(b, uint64(50_000_000+p))
		// Two repeated lines, unchanged.
		a, b = append(a, repeated, repeated), append(b, repeated, repeated)
		unchanged += 2
	}
	match := AlignHashes(a, b)
	pairs, last := 0, -1
	for i, j := range match {
		if j < 0 {
			continue
		}
		if a[i] != b[j] || j <= last {
			t.Fatalf("bad pair a[%d] -> b[%d]", i, j)
		}
		last = j
		pairs++
	}
	if pairs != unchanged {
		t.Errorf("%d of %d unchanged lines were paired", pairs, unchanged)
	}
}

func randomSequence(rng *rand.Rand, n, alphabet int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = uint64(rng.Intn(alphabet))
	}
	return out
}

func lcsLength(a, b []uint64) int {
	prev := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev = cur
	}
	return prev[len(b)]
}
