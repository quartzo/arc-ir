package verifier

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"arc-ir/pkg/ir"
	"arc-ir/pkg/parser"
)

func mustParse(t *testing.T, src string) *ir.Program {
	t.Helper()
	prog, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return prog
}

const loopSrc = `
entry:
    r1 = ARENA_CREATE CXL_POOLED, 128
    r2 = ARENA_ALLOC r1, [i64; 16]
    r3 = 0
loop:
    JGE r3, 10, done
    STORE [r2 + r3*8], r3
    r3 = ADD r3, 1
    JMP loop
done:
    r4 = LOAD [r2 + r3*8 - 8]
    PRINT r4
    RET r4
`

func TestVerifyLoop(t *testing.T) {
	res, err := Verify(mustParse(t, loopSrc))
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if res.Blocks != 3 {
		t.Errorf("blocks = %d, want 3", res.Blocks)
	}
}

func TestVerifyStraightLine(t *testing.T) {
	src := "entry:\n" +
		"    r1 = 10\n" +
		"    r2 = 20\n" +
		"    r3 = ADD r1, r2\n" +
		"    PRINT r3\n" +
		"    RET r3\n"
	if _, err := Verify(mustParse(t, src)); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestVerifyRejections(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"out of bounds static",
			"entry:\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 2]\n" +
				"    r3 = 7\n" +
				"    STORE [r2 + 12], r3\n" +
				"    RET\n",
			"out-of-bounds",
		},
		{
			"uninitialized register",
			"entry:\n" +
				"    PRINT r5\n" +
				"    RET\n",
			"uninitialized",
		},
		{
			"type mismatch",
			"entry:\n" +
				"    r1 = 1\n" +
				"    r2 = LOAD [r1]\n" +
				"    RET\n",
			"expected data pointer",
		},
		{
			"unprovable dynamic bounds",
			"entry:\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
				"    r3 = 100\n" +
				"    STORE [r2 + r3], r3\n" +
				"    RET\n",
			"out-of-bounds",
		},
		{
			"allocation exceeds arena",
			"entry:\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 8\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
				"    RET\n",
			"exceeds capacity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestVerifyBranchRefinement checks that ranges narrowed on branch edges make
// an otherwise-unprovable access safe.
func TestVerifyBranchRefinement(t *testing.T) {
	// r3 starts as an unknown scalar (from a load). Two guards narrow it to
	// [0,7] before the store, so [r2 + r3*8] is provably in bounds.
	safe := "entry:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = LOAD [r2]\n" +
		"    JLT r3, 0, out\n" +
		"    JGE r3, 8, out\n" +
		"    STORE [r2 + r3*8], r3\n" +
		"    RET\n" +
		"out:\n" +
		"    RET\n"
	if _, err := Verify(mustParse(t, safe)); err != nil {
		t.Fatalf("expected the refined branches to verify, got %v", err)
	}

	// Without the lower-bound guard the offset may be negative: rejected.
	unsafe := "entry:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = LOAD [r2]\n" +
		"    JGE r3, 8, out\n" +
		"    STORE [r2 + r3*8], r3\n" +
		"    RET\n" +
		"out:\n" +
		"    RET\n"
	if _, err := Verify(mustParse(t, unsafe)); err == nil {
		t.Fatal("expected the unbounded access to be rejected")
	}
}

func TestVerifyFunctions(t *testing.T) {
	src := "func main -> i64\n" +
		"    r1 = 3\n" +
		"    r2 = 4\n" +
		"    CALL add\n" +
		"    PRINT r0\n" +
		"    RET r0\n" +
		"endfunc\n" +
		"func add(r1: i64, r2: i64) -> i64\n" +
		"    r0 = ADD r1, r2\n" +
		"    RET r0\n" +
		"endfunc\n"
	res, err := Verify(mustParse(t, src))
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if res.Funcs != 2 {
		t.Errorf("funcs = %d, want 2", res.Funcs)
	}
}

func TestVerifyRecursion(t *testing.T) {
	src := "func main -> i64\n" +
		"    r1 = 5\n" +
		"    CALL fact\n" +
		"    RET r0\n" +
		"endfunc\n" +
		"func fact(r1: i64) -> i64\n" +
		"    JGT r1, 1, rec\n" +
		"    r0 = 1\n" +
		"    RET r0\n" +
		"rec:\n" +
		"    r2 = SUB r1, 1\n" +
		"    s0 = r1\n" +
		"    r1 = r2\n" +
		"    CALL fact\n" +
		"    r0 = MUL s0, r0\n" +
		"    RET r0\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, src)); err != nil {
		t.Fatalf("expected recursion to verify, got %v", err)
	}
}

func TestVerifyPointerParam(t *testing.T) {
	src := "func main -> i64\n" +
		"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r2 = ARENA_ALLOC s0, [i64; 8]\n" +
		"    r1 = r2\n" +
		"    CALL read\n" +
		"    RET r0\n" +
		"endfunc\n" +
		"func read(r1: ptr([i64; 8])) -> i64\n" +
		"    r0 = LOAD [r1]\n" +
		"    RET r0\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, src)); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestVerifyFunctionRejections(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"argument type mismatch",
			"func main -> i64\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    CALL f\n" +
				"    RET\n" +
				"endfunc\n" +
				"func f(r1: i64) -> i64\n" +
				"    r0 = r1\n" +
				"    RET r0\n" +
				"endfunc\n",
			"expected an integer",
		},
		{
			"unknown function",
			"func main -> i64\n" +
				"    CALL nope\n" +
				"    RET\n" +
				"endfunc\n",
			"undefined function",
		},
		{
			"pointer argument too small",
			"func main -> i64\n" +
				"    r5 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    r2 = ARENA_ALLOC r5, [i64; 2]\n" +
				"    r1 = r2\n" +
				"    CALL f\n" +
				"    RET r0\n" +
				"endfunc\n" +
				"func f(r1: ptr([i64; 8])) -> i64\n" +
				"    r0 = LOAD [r1]\n" +
				"    RET r0\n" +
				"endfunc\n",
			"smaller than the required",
		},
		{
			"out of bounds through pointer parameter",
			"func main -> i64\n" +
				"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    r2 = ARENA_ALLOC s0, [i64; 8]\n" +
				"    r1 = r2\n" +
				"    CALL f\n" +
				"    RET r0\n" +
				"endfunc\n" +
				"func f(r1: ptr([i64; 2])) -> i64\n" +
				"    r0 = LOAD [r1 + 32]\n" +
				"    RET r0\n" +
				"endfunc\n",
			"out-of-bounds",
		},
		{
			"falls off the end (scalar)",
			"func main -> i64\n" +
				"    r1 = 1\n" +
				"endfunc\n",
			"may fall off its end",
		},
		{
			"falls off the end (pointer return)",
			// A `-> ptr` function that reaches the end returns 0 while the
			// caller taints r0 as a live pointer: dereferencing it would be a
			// null dereference. This must be rejected.
			"func main -> i64\n" +
				"    CALL f\n" +
				"    r1 = LOAD [r0]\n" +
				"    RET\n" +
				"endfunc\n" +
				"func f() -> ptr([i64; 8])\n" +
				"    r1 = 1\n" +
				"endfunc\n",
			"may fall off its end",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyExplicitTerminator(t *testing.T) {
	// A function may end with JMP (a loop that never returns): it does not fall
	// off its end, so it verifies.
	src := "func main -> i64\n" +
		"loop:\n" +
		"    r1 = 1\n" +
		"    JMP loop\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, src)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyArenaMerge(t *testing.T) {
	// The same arena on both paths merges to that arena: the handle is still
	// usable.
	same := "func main -> i64\n" +
		"    s0 = ARENA_CREATE DRAM_LOCAL, 16\n" +
		"    r1 = 0\n" +
		"    JEQ r1, 0, b\n" +
		"    s2 = s0\n" +
		"    JMP done\n" +
		"b:\n" +
		"    s2 = s0\n" +
		"done:\n" +
		"    r2 = ARENA_ALLOC s2, [i64; 1]\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, same)); err != nil {
		t.Fatalf("same arena: expected success, got %v", err)
	}

	// Two different arena sites feeding one register make an ambiguous handle
	// ("one of several arenas"), which is rejected at the join. The rejection
	// must not depend on the fixpoint's edge order.
	cases := []struct {
		name string
		src  string
	}{
		{
			"two create sites",
			"func main -> i64\n" +
				"    r1 = 0\n" +
				"    JEQ r1, 0, b\n" +
				"    s0 = ARENA_CREATE DRAM_LOCAL, 16\n" +
				"    JMP done\n" +
				"b:\n" +
				"    s0 = ARENA_CREATE DRAM_LOCAL, 16\n" +
				"done:\n" +
				"    r2 = ARENA_ALLOC s0, [i64; 1]\n" +
				"    RET\n" +
				"endfunc\n",
		},
		{
			"parameter or local arena",
			"func main -> i64\n" +
				"    s0 = ARENA_CREATE DRAM_LOCAL, 32\n" +
				"    r1 = s0\n" +
				"    CALL helper\n" +
				"    RET\n" +
				"endfunc\n" +
				"func helper(r1: arena(32)) -> i64\n" +
				"    r2 = 0\n" +
				"    JEQ r2, 0, local\n" +
				"    s0 = r1\n" +
				"    JMP use\n" +
				"local:\n" +
				"    s0 = ARENA_CREATE DRAM_LOCAL, 32\n" +
				"use:\n" +
				"    RET\n" +
				"endfunc\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 20; i++ {
				_, err := Verify(mustParse(t, tc.src))
				if err == nil {
					t.Fatalf("iteration %d: expected rejection, got success", i)
				}
				if !strings.Contains(err.Error(), "may be one of several arenas") {
					t.Fatalf("error = %q, want substring %q", err.Error(), "may be one of several arenas")
				}
			}
		})
	}
}

func TestVerifyNonScalarReturns(t *testing.T) {
	// A pointer into a caller-owned region can be returned: the region outlives
	// the call.
	src := "func main -> i64\n" +
		"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r1 = s0\n" +
		"    CALL make\n" +
		"    r2 = r0\n" +
		"    r3 = 99\n" +
		"    STORE [r2], r3\n" +
		"    r4 = LOAD [r2]\n" +
		"    PRINT r4\n" +
		"    RET r4\n" +
		"endfunc\n" +
		"func make(r1: arena) -> ptr([i64; 8])\n" +
		"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET r0\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, src)); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestVerifyPathSensitiveProvenance(t *testing.T) {
	// Two paths allocate from the same caller-owned arena and their pointers
	// merge at a join. The arena identity survives the join, so the pointer is
	// still returnable; only the allocation identity is dropped.
	src := "func main -> i64\n" +
		"    s0 = ARENA_CREATE DRAM_LOCAL, 128\n" +
		"    r1 = s0\n" +
		"    CALL pick\n" +
		"    RET\n" +
		"endfunc\n" +
		"func pick(r1: arena) -> ptr([i64; 8])\n" +
		"    r3 = 0\n" +
		"    JEQ r3, 0, b\n" +
		"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    JMP done\n" +
		"b:\n" +
		"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
		"done:\n" +
		"    RET r0\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, src)); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestVerifyReturnRejections(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"wrong return type",
			"func main -> i64\n" +
				"    CALL f\n" +
				"    r1 = 0\n" +
				"    RET r1\n" +
				"endfunc\n" +
				"func f() -> ptr([i64; 8])\n" +
				"    r0 = 5\n" +
				"    RET r0\n" +
				"endfunc\n",
			"declares return type ptr",
		},
		{
			"returned allocation too small",
			"func main -> i64\n" +
				"    s0 = ARENA_CREATE DRAM_LOCAL, 16\n" +
				"    r1 = s0\n" +
				"    CALL f\n" +
				"    r1 = 0\n" +
				"    RET r1\n" +
				"endfunc\n" +
				"func f(r1: arena) -> ptr([i64; 8])\n" +
				"    r0 = ARENA_ALLOC r1, [i64; 2]\n" +
				"    RET r0\n" +
				"endfunc\n",
			"16-byte allocation",
		},
		{
			"bare RET for non-scalar",
			"func main -> i64\n" +
				"    CALL f\n" +
				"    r1 = 0\n" +
				"    RET r1\n" +
				"endfunc\n" +
				"func f() -> ptr([i64; 8])\n" +
				"    RET\n" +
				"endfunc\n",
			"RET has no value",
		},
		{
			"return a pointer into a local region",
			"func main -> i64\n" +
				"    r1 = 0\n" +
				"    RET r1\n" +
				"endfunc\n" +
				"func f() -> ptr([i64; 8])\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
				"    RET r0\n" +
				"endfunc\n",
			"does not outlive the function",
		},
		{
			// Conservative limitation: when the two paths point into *different*
			// caller-owned arenas the merge gives unknown provenance, so the
			// pointer is rejected. A provenance set (not a single arena id)
			// would accept it; see the roadmap.
			"merged pointer from two borrowed arenas",
			"func main -> i64\n" +
				"    s0 = ARENA_CREATE DRAM_LOCAL, 256\n" +
				"    r1 = s0\n" +
				"    r2 = s0\n" +
				"    CALL pick\n" +
				"    RET\n" +
				"endfunc\n" +
				"func pick(r1: arena, r2: arena) -> ptr([i64; 8])\n" +
				"    r3 = 0\n" +
				"    JEQ r3, 0, b\n" +
				"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
				"    JMP done\n" +
				"b:\n" +
				"    r0 = ARENA_ALLOC r2, [i64; 8]\n" +
				"done:\n" +
				"    RET r0\n" +
				"endfunc\n",
			"does not outlive the function",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyOwnership(t *testing.T) {
	// A borrowed region: the callee allocates from it; the caller releases it.
	valid := "func main -> i64\n" +
		"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r1 = s0\n" +
		"    CALL fill\n" +
		"    RET\n" +
		"endfunc\n" +
		"func fill(r1: arena) -> i64\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = 1\n" +
		"    STORE [r2], r3\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected the borrowed-region pattern to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"local pointer escapes into foreign memory",
			"struct Box\n" +
				"    p: ptr([i64; 8])\n" +
				"endstruct\n" +
				"func main -> i64\n" +
				"    s0 = ARENA_CREATE DRAM_LOCAL, 256\n" +
				"    s1 = ARENA_ALLOC s0, Box\n" +
				"    s2 = ARENA_ALLOC s0, [i64; 8]\n" +
				"    s1.p = s2\n" +
				"    r1 = s1\n" +
				"    CALL fill\n" +
				"    RET\n" +
				"endfunc\n" +
				"func fill(r1: ptr(Box)) -> i64\n" +
				"    r2 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    r3 = ARENA_ALLOC r2, [i64; 8]\n" +
				"    r1.p = r3\n" +
				"    RET\n" +
				"endfunc\n",
			"outlives the function",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyTypedMemory(t *testing.T) {
	// A pointer stored in a struct field comes back as a data pointer.
	valid := "struct Box\n" +
		"    p: ptr([i64; 1])\n" +
		"endstruct\n" +
		"func main -> i64\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
		"    r2 = ARENA_ALLOC r1, Box\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 1]\n" +
		"    r4 = 99\n" +
		"    STORE [r3], r4\n" +
		"    r2.p = r3\n" +
		"    r5 = r2.p\n" +
		"    r6 = LOAD [r5]\n" +
		"    PRINT r6\n" +
		"    RET r6\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected typed memory to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"store arena in an integer array",
			"func main -> i64\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 16]\n" +
				"    STORE [r2], r1\n" +
				"    RET\n" +
				"endfunc\n",
			"array element expects an integer",
		},
		{
			"store pointer in an integer array",
			"func main -> i64\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 16]\n" +
				"    r3 = ARENA_ALLOC r1, [i64; 1]\n" +
				"    STORE [r2], r3\n" +
				"    RET\n" +
				"endfunc\n",
			"array element expects an integer",
		},
		{
			"integer element is not a pointer",
			"func main -> i64\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 16]\n" +
				"    r5 = LOAD [r2]\n" +
				"    r6 = LOAD [r5]\n" +
				"    RET\n" +
				"endfunc\n",
			"expected data pointer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyCapacity(t *testing.T) {
	// Cumulative over-allocation is rejected, even though each allocation fits.
	over := "func main -> i64\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 128\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r4 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, over)); err == nil || !strings.Contains(err.Error(), "arena exhausted") {
		t.Fatalf("expected cumulative exhaustion, got %v", err)
	}

	// An exact fit is accepted.
	fits := "func main -> i64\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 128\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, fits)); err != nil {
		t.Fatalf("expected exact fit to verify, got %v", err)
	}

	// Capacity is enforced across a function boundary via arena(<size>).
	across := "func main -> i64\n" +
		"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r1 = s0\n" +
		"    CALL fill\n" +
		"    RET\n" +
		"endfunc\n" +
		"func fill(r1: arena(64)) -> i64\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, across)); err == nil || !strings.Contains(err.Error(), "arena exhausted") {
		t.Fatalf("expected cross-function exhaustion, got %v", err)
	}
}

func FuzzVerify(f *testing.F) {
	f.Add("entry:\n    r1 = 1\n    RET\n")
	f.Add("func main -> i64\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    RET\n" +
		"endfunc\n")
	f.Add("func main -> i64\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET\n" +
		"endfunc\n")
	f.Fuzz(func(t *testing.T, src string) {
		prog, err := parser.Parse(src)
		if err != nil {
			return
		}
		// The verifier must never panic or hang on arbitrary input.
		_, _ = Verify(prog)
	})
}

func TestVerifyGrowableArena(t *testing.T) {
	// Many allocations exceed any single chunk; the arena grows.
	valid := "func main -> i64\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, growable(64)\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r4 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected growable to verify, got %v", err)
	}

	// A growable arena satisfies any declared parameter capacity.
	across := "func main -> i64\n" +
		"    s0 = ARENA_CREATE DRAM_LOCAL, growable(64)\n" +
		"    r1 = s0\n" +
		"    CALL fill\n" +
		"    RET\n" +
		"endfunc\n" +
		"func fill(r1: arena(4096)) -> i64\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, across)); err != nil {
		t.Fatalf("expected growable to satisfy arena(4096), got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"allocation larger than chunk",
			"func main -> i64\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, growable(64)\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 16]\n" +
				"    RET\n" +
				"endfunc\n",
			"exceeds chunk size",
		},
		{
			"fixed arena smaller than declared parameter capacity",
			"func main -> i64\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    CALL fill\n" +
				"    RET\n" +
				"endfunc\n" +
				"func fill(r1: arena(4096)) -> i64\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
				"    RET\n" +
				"endfunc\n",
			"smaller than the declared",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyStackArena(t *testing.T) {
	// A stack arena is fixed: cumulative allocation is proven, the allocation
	// is annotated for inline lowering, and the arena is never destroyed.
	valid := "func main -> i64\n" +
		"    r1 = ARENA_CREATE STACK, 128\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET\n" +
		"endfunc\n"
	res, err := Verify(mustParse(t, valid))
	if err != nil {
		t.Fatalf("expected stack arena to verify, got %v", err)
	}
	allocs, destroys := 0, 0
	for _, f := range res.Module.Funcs {
		for _, b := range f.Blocks {
			for _, in := range b.Instrs {
				switch in.Op {
				case ir.OpArenaAlloc:
					allocs++
					if !in.Stack {
						t.Errorf("stack allocation not annotated: %s", in)
					}
				case ir.OpArenaDestroy:
					destroys++
				}
			}
		}
	}
	if allocs != 2 {
		t.Fatalf("found %d allocations, want 2", allocs)
	}
	if destroys != 0 {
		t.Fatalf("stack arena must not be destroyed, found %d ARENA_DESTROY", destroys)
	}

	// A pointer into a stack arena may be passed to a callee: the region (the
	// frame) outlives the call.
	ptrArg := "func main -> i64\n" +
		"    r1 = ARENA_CREATE STACK, 64\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r1 = r2\n" +
		"    CALL read\n" +
		"    RET r0\n" +
		"endfunc\n" +
		"func read(r1: ptr([i64; 8])) -> i64\n" +
		"    r0 = LOAD [r1]\n" +
		"    RET r0\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, ptrArg)); err != nil {
		t.Fatalf("expected pointer into a stack arena to be passable, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"growable stack is rejected",
			"func main -> i64\n" +
				"    r1 = ARENA_CREATE STACK, growable(64)\n" +
				"    RET\n" +
				"endfunc\n",
			"cannot be growable",
		},
		{
			"capacity is enforced",
			"func main -> i64\n" +
				"    r1 = ARENA_CREATE STACK, 64\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
				"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
				"    RET\n" +
				"endfunc\n",
			"arena exhausted",
		},
		{
			"a stack arena cannot be passed",
			"func main -> i64\n" +
				"    r1 = ARENA_CREATE STACK, 64\n" +
				"    CALL f\n" +
				"    RET\n" +
				"endfunc\n" +
				"func f(r1: arena) -> i64\n" +
				"    RET\n" +
				"endfunc\n",
			"cannot be passed",
		},
		{
			"a stack pointer cannot be returned",
			"func main -> i64\n" +
				"    RET\n" +
				"endfunc\n" +
				"func f(r1: arena) -> ptr([i64; 8])\n" +
				"    r1 = ARENA_CREATE STACK, 64\n" +
				"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
				"    RET r0\n" +
				"endfunc\n",
			"does not outlive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyStackParam(t *testing.T) {
	// A callee with a stack(N) parameter borrows a budget of N bytes from the
	// caller's frame and allocates inline (the allocation is annotated).
	valid := "func main -> i64\n" +
		"    s0 = ARENA_CREATE STACK, 64\n" +
		"    r1 = s0\n" +
		"    CALL fill\n" +
		"    RET\n" +
		"endfunc\n" +
		"func fill(r1: stack(64)) -> ptr([i64; 8])\n" +
		"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET r0\n" +
		"endfunc\n"
	res, err := Verify(mustParse(t, valid))
	if err != nil {
		t.Fatalf("expected stack parameter to verify, got %v", err)
	}
	annotated := false
	for _, f := range res.Module.Funcs {
		for _, b := range f.Blocks {
			for _, in := range b.Instrs {
				if in.Op == ir.OpArenaAlloc && in.Stack {
					annotated = true
				}
			}
		}
	}
	if !annotated {
		t.Error("stack allocation in the callee was not annotated for inline lowering")
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"budget larger than the arena",
			"func main -> i64\n" +
				"    s0 = ARENA_CREATE STACK, 32\n" +
				"    r1 = s0\n" +
				"    CALL fill\n" +
				"    RET\n" +
				"endfunc\n" +
				"func fill(r1: stack(64)) -> i64\n" +
				"    RET\n" +
				"endfunc\n",
			"remaining capacity",
		},
		{
			"repeated calls consume the budget",
			"func main -> i64\n" +
				"    s0 = ARENA_CREATE STACK, 16\n" +
				"    r1 = s0\n" +
				"    CALL fill\n" +
				"    r1 = s0\n" +
				"    CALL fill\n" +
				"    r1 = s0\n" +
				"    CALL fill\n" +
				"    RET\n" +
				"endfunc\n" +
				"func fill(r1: stack(8)) -> i64\n" +
				"    r0 = ARENA_ALLOC r1, [i64; 1]\n" +
				"    RET r0\n" +
				"endfunc\n",
			"remaining capacity",
		},
		{
			"a heap arena does not match a stack parameter",
			"func main -> i64\n" +
				"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    r1 = s0\n" +
				"    CALL fill\n" +
				"    RET\n" +
				"endfunc\n" +
				"func fill(r1: stack(64)) -> i64\n" +
				"    RET\n" +
				"endfunc\n",
			"expected a stack arena",
		},
		{
			"a stack arena does not match a heap parameter",
			"func main -> i64\n" +
				"    s0 = ARENA_CREATE STACK, 64\n" +
				"    r1 = s0\n" +
				"    CALL fill\n" +
				"    RET\n" +
				"endfunc\n" +
				"func fill(r1: arena) -> i64\n" +
				"    RET\n" +
				"endfunc\n",
			"cannot be passed to a heap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyFloat(t *testing.T) {
	valid := "func main -> i64\n" +
		"    v1 = 2.5\n" +
		"    v2 = 4.0\n" +
		"    v3 = FMUL v1, v2\n" +
		"    v4 = FADD v3, v1\n" +
		"    PRINTF v4\n" +
		"    r1 = FTOI v4\n" +
		"    RET r1\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected float program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"float as pointer", "func main -> i64\n" +
			"    v1 = 1.0\n" +
			"    r1 = LOAD [v1]\n" +
			"    RET\n" +
			"endfunc\n", "expected data pointer"},
		{"FADD on scalar", "func main -> i64\n" +
			"    r1 = 1\n" +
			"    v1 = FADD r1, r1\n" +
			"    RET\n" +
			"endfunc\n", "expected a float"},
		{"ITOF on float", "func main -> i64\n" +
			"    v1 = 1.0\n" +
			"    v2 = ITOF v1\n" +
			"    RET\n" +
			"endfunc\n", "expected integer"},
		{"FTOI into vector", "func main -> i64\n" +
			"    v1 = 1.0\n" +
			"    v2 = FTOI v1\n" +
			"    RET\n" +
			"endfunc\n", "requires a general register"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyF32(t *testing.T) {
	valid := "func main -> i64\n" +
		"    v1 = f32(2.5)\n" +
		"    v2 = f32(4.0)\n" +
		"    v3 = FMUL v1, v2\n" +
		"    v4 = FADD v3, v1\n" +
		"    PRINTF v4\n" +
		"    r1 = FTOI v4\n" +
		"    RET r1\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected f32 program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"mixing f32 and f64", "func main -> i64\n" +
			"    v1 = 1.0\n" +
			"    v2 = f32(1.0)\n" +
			"    v3 = FADD v1, v2\n" +
			"    RET\n" +
			"endfunc\n", "expected f64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifySIMD(t *testing.T) {
	valid := "func main -> i64\n" +
		"    v1 = f32x4(1.0, 2.0, 3.0, 4.0)\n" +
		"    v2 = f32x4(10.0, 20.0, 30.0, 40.0)\n" +
		"    v3 = VADD v1, v2\n" +
		"    PRINTV v3\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected simd program to verify, got %v", err)
	}

	valid64 := "func main -> i64\n" +
		"    v1 = f64x2(1.5, 2.5)\n" +
		"    v2 = f64x2(10.0, 20.0)\n" +
		"    v3 = VADD v1, v2\n" +
		"    PRINTV v3\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid64)); err != nil {
		t.Fatalf("expected f64x2 program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"mixing kinds", "func main -> i64\n" +
			"    v1 = f32x4(1.0)\n" +
			"    v2 = i32x4(1)\n" +
			"    v3 = VADD v1, v2\n" +
			"    RET\n" +
			"endfunc\n", "expected f32x4"},
		{"scalar op on vector", "func main -> i64\n" +
			"    v1 = f32x4(1.0)\n" +
			"    r1 = ADD v1, v1\n" +
			"    RET\n" +
			"endfunc\n", "expected integer"},
		{"packed 64-bit multiply", "func main -> i64\n" +
			"    v1 = i64x2(1)\n" +
			"    v2 = VMUL v1, v1\n" +
			"    RET\n" +
			"endfunc\n", "not available"},
		{"packed integer divide", "func main -> i64\n" +
			"    v1 = i32x4(1)\n" +
			"    v2 = VDIV v1, v1\n" +
			"    RET\n" +
			"endfunc\n", "not available"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyStruct(t *testing.T) {
	valid := "struct Node\n" +
		"    next: ptr([i64; 1])\n" +
		"    val: i64\n" +
		"endstruct\n" +
		"func main -> i64\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
		"    r2 = ARENA_ALLOC r1, Node\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 1]\n" +
		"    r4 = 42\n" +
		"    STORE [r3], r4\n" +
		"    r2.next = r3\n" +
		"    r5 = 7\n" +
		"    r2.val = r5\n" +
		"    r6 = r2.next\n" +
		"    r7 = LOAD [r6]\n" +
		"    PRINT r7\n" +
		"    RET r7\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected struct program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"uninitialized field", "struct N\n" +
			"    a: i64\n" +
			"endstruct\n" +
			"func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, N\n" +
			"    r3 = r2.a\n" +
			"    RET\n" +
			"endfunc\n", "not initialized"},
		{"wrong field type", "struct N\n" +
			"    p: ptr([i64; 1])\n" +
			"endstruct\n" +
			"func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, N\n" +
			"    r3 = 5\n" +
			"    r2.p = r3\n" +
			"    RET\n" +
			"endfunc\n", "field expects"},
		{"field on non-struct", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [i64; 1]\n" +
			"    r3 = r2.x\n" +
			"    RET\n" +
			"endfunc\n", "struct or enum pointer"},
		{"unknown field", "struct N\n" +
			"    a: i64\n" +
			"endstruct\n" +
			"func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, N\n" +
			"    r3 = r2.b\n" +
			"    RET\n" +
			"endfunc\n", "no field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyPtrStruct(t *testing.T) {
	valid := "struct Node\n" +
		"    next: ptr([i64; 1])\n" +
		"    val: i64\n" +
		"endstruct\n" +
		"func main -> i64\n" +
		"    s0 = ARENA_CREATE DRAM_LOCAL, 256\n" +
		"    r2 = ARENA_ALLOC s0, Node\n" +
		"    r3 = ARENA_ALLOC s0, [i64; 1]\n" +
		"    r4 = 42\n" +
		"    STORE [r3], r4\n" +
		"    r2.next = r3\n" +
		"    r5 = 7\n" +
		"    r2.val = r5\n" +
		"    r1 = r2\n" +
		"    CALL read\n" +
		"    PRINT r0\n" +
		"    RET r0\n" +
		"endfunc\n" +
		"func read(r1: ptr(Node)) -> i64\n" +
		"    r2 = r1.next\n" +
		"    r0 = LOAD [r2]\n" +
		"    RET r0\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected ptr(Node) program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"raw ptr for struct", "struct N\n" +
			"    a: i64\n" +
			"endstruct\n" +
			"func main -> i64\n" +
			"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC s0, [i64; 8]\n" +
			"    r1 = r2\n" +
			"    CALL f\n" +
			"    RET\n" +
			"endfunc\n" +
			"func f(r1: ptr(N)) -> i64\n" +
			"    RET\n" +
			"endfunc\n", "expected a N pointer"},
		{"uninitialized struct", "struct N\n" +
			"    a: i64\n" +
			"endstruct\n" +
			"func main -> i64\n" +
			"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC s0, N\n" +
			"    r1 = r2\n" +
			"    CALL f\n" +
			"    RET\n" +
			"endfunc\n" +
			"func f(r1: ptr(N)) -> i64\n" +
			"    RET\n" +
			"endfunc\n", "not initialized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyArray(t *testing.T) {
	valid := []struct {
		name string
		src  string
	}{
		{"i64 array", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [i64; 4]\n" +
			"    r3 = 7\n" +
			"    STORE [r2 + 16], r3\n" +
			"    r4 = LOAD [r2 + 16]\n" +
			"    PRINT r4\n" +
			"    RET r4\n" +
			"endfunc\n"},
		{"f64 array", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [f64; 4]\n" +
			"    v1 = 2.5\n" +
			"    STORE [r2 + 8], v1\n" +
			"    v2 = LOAD [r2 + 8]\n" +
			"    PRINTF v2\n" +
			"    RET\n" +
			"endfunc\n"},
		{"array parameter", "func main -> i64\n" +
			"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC s0, [i64; 8]\n" +
			"    r1 = r2\n" +
			"    CALL read\n" +
			"    RET r0\n" +
			"endfunc\n" +
			"func read(r1: ptr([i64; 8])) -> i64\n" +
			"    r0 = LOAD [r1]\n" +
			"    RET r0\n" +
			"endfunc\n"},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(mustParse(t, tc.src)); err != nil {
				t.Fatalf("expected program to verify, got %v", err)
			}
		})
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"wrong element type", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [f64; 4]\n" +
			"    r3 = 7\n" +
			"    STORE [r2], r3\n" +
			"    RET\n" +
			"endfunc\n", "array element expects"},
		{"f64 element needs vector register", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [f64; 4]\n" +
			"    r3 = LOAD [r2]\n" +
			"    RET\n" +
			"endfunc\n", "requires a vector register"},
		{"i64 element needs general register", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [i64; 4]\n" +
			"    v1 = LOAD [r2]\n" +
			"    RET\n" +
			"endfunc\n", "requires a general register"},
		{"array out of bounds", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [i64; 4]\n" +
			"    r3 = LOAD [r2 + 32]\n" +
			"    RET\n" +
			"endfunc\n", "out-of-bounds"},
		{"array argument too small", "func main -> i64\n" +
			"    r5 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r5, [i64; 2]\n" +
			"    r1 = r2\n" +
			"    CALL f\n" +
			"    RET\n" +
			"endfunc\n" +
			"func f(r1: ptr([i64; 8])) -> i64\n" +
			"    RET\n" +
			"endfunc\n", "smaller than the required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyWidths(t *testing.T) {
	valid := []struct {
		name string
		src  string
	}{
		{"u8 array", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [u8; 8]\n" +
			"    r3 = 200\n" +
			"    STORE [r2 + 3], r3\n" +
			"    r4 = LOAD [r2 + 3]\n" +
			"    PRINT r4\n" +
			"    RET r4\n" +
			"endfunc\n"},
		{"i8 array sign extends", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [i8; 4]\n" +
			"    r3 = 1\n" +
			"    STORE [r2], r3\n" +
			"    r4 = LOAD [r2]\n" +
			"    RET r4\n" +
			"endfunc\n"},
		{"mixed-width struct", "struct R\n" +
			"    a: u8\n" +
			"    b: u16\n" +
			"    c: u32\n" +
			"    d: u64\n" +
			"endstruct\n" +
			"func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, R\n" +
			"    r3 = 7\n" +
			"    r2.a = r3\n" +
			"    r2.b = r3\n" +
			"    r2.c = r3\n" +
			"    r2.d = r3\n" +
			"    r4 = r2.c\n" +
			"    RET r4\n" +
			"endfunc\n"},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(mustParse(t, tc.src)); err != nil {
				t.Fatalf("expected program to verify, got %v", err)
			}
		})
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"float into integer array", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [u8; 8]\n" +
			"    v1 = 1.5\n" +
			"    STORE [r2], v1\n" +
			"    RET\n" +
			"endfunc\n", "array element expects an integer"},
		{"float element into gp", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [f64; 2]\n" +
			"    r3 = LOAD [r2]\n" +
			"    RET\n" +
			"endfunc\n", "requires a vector register"},
		{"out of bounds by width", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [u8; 8]\n" +
			"    r3 = LOAD [r2 + 8]\n" +
			"    RET\n" +
			"endfunc\n", "out-of-bounds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyUnsignedCompare(t *testing.T) {
	// A u8 value of 200 is > 100 unsigned; the branch is taken.
	src := "func main -> i64\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r2 = ARENA_ALLOC r1, [u8; 2]\n" +
		"    r3 = 200\n" +
		"    STORE [r2], r3\n" +
		"    r4 = LOAD [r2]\n" +
		"    r0 = 0\n" +
		"    JGT r4, 100, big\n" +
		"    r0 = 1\n" +
		"big:\n" +
		"    RET r0\n" +
		"endfunc\n"
	res, err := Verify(mustParse(t, src))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// The comparison must have been lowered to its unsigned form.
	found := false
	for _, b := range res.Module.Funcs[0].Blocks {
		for _, in := range b.Instrs {
			if in.Op == ir.OpJgtu {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected the comparison to lower to JGTU")
	}
}

func TestVerifyEnum(t *testing.T) {
	valid := "enum V\n" +
		"    A(x: i64)\n" +
		"    B(y: i32)\n" +
		"endenum\n" +
		"func main -> i64\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r2 = ARENA_ALLOC r1, V\n" +
		"    VARIANT r2, V.A\n" +
		"    r3 = 7\n" +
		"    r2.x = r3\n" +
		"    r4 = r2.x\n" +
		"    PRINT r4\n" +
		"    RET r4\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected enum program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"wrong variant field", "enum V\n" +
			"    A(x: i64)\n" +
			"    B(y: i32)\n" +
			"endenum\n" +
			"func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, V\n" +
			"    VARIANT r2, V.A\n" +
			"    r3 = r2.y\n" +
			"    RET\n" +
			"endfunc\n", "V has no field"},
		{"unknown variant", "enum V\n" +
			"    A(x: i64)\n" +
			"    B(y: i32)\n" +
			"endenum\n" +
			"func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, V\n" +
			"    r3 = r2.x\n" +
			"    RET\n" +
			"endfunc\n", "unknown active variant"},
		{"uninitialized payload", "enum V\n" +
			"    A(x: i64)\n" +
			"    B(y: i32)\n" +
			"endenum\n" +
			"func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, V\n" +
			"    VARIANT r2, V.A\n" +
			"    r3 = r2.x\n" +
			"    RET\n" +
			"endfunc\n", "not initialized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(mustParse(t, tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestVerifyBlocks(t *testing.T) {
	// A scratch register the callee writes is destroyed for the caller.
	bad := "func main -> i64\n" +
		"    r5 = 42\n" +
		"    CALL leaf\n" +
		"    PRINT r5\n" +
		"    RET\n" +
		"endfunc\n" +
		"func leaf -> i64\n" +
		"    r5 = 7\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, bad)); err == nil || !strings.Contains(err.Error(), "destroyed register") {
		t.Fatalf("expected a destroyed-register error, got %v", err)
	}

	// A preserved register survives a call that writes it.
	ok := "func main -> i64\n" +
		"    s0 = 42\n" +
		"    CALL leaf\n" +
		"    PRINT s0\n" +
		"    RET\n" +
		"endfunc\n" +
		"func leaf -> i64\n" +
		"    s0 = 7\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, ok)); err != nil {
		t.Fatalf("expected the preserved register to survive, got %v", err)
	}

	// A scratch register is destroyed by any call, even one that does not write
	// it: scratch is volatile.
	vol := "func main -> i64\n" +
		"    r5 = 42\n" +
		"    CALL leaf\n" +
		"    PRINT r5\n" +
		"    RET\n" +
		"endfunc\n" +
		"func leaf -> i64\n" +
		"    r6 = 7\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, vol)); err == nil || !strings.Contains(err.Error(), "destroyed register") {
		t.Fatalf("expected a destroyed-register error for volatile scratch, got %v", err)
	}

	// The vector file is volatile too: a call destroys every vector register.
	vec := "func main -> i64\n" +
		"    v1 = 2.5\n" +
		"    CALL leaf\n" +
		"    PRINTF v1\n" +
		"    RET\n" +
		"endfunc\n" +
		"func leaf -> i64\n" +
		"    v1 = 1.0\n" +
		"    RET\n" +
		"endfunc\n"
	if _, err := Verify(mustParse(t, vec)); err == nil || !strings.Contains(err.Error(), "destroyed register") {
		t.Fatalf("expected a destroyed-register error for volatile vectors, got %v", err)
	}
}

func TestVerifyErrorLine(t *testing.T) {
	src := "entry:\n    PRINT r5\n    RET\n"
	_, err := Verify(mustParse(t, src))
	if err == nil {
		t.Fatal("expected error")
	}
	ve, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *verifier.Error", err)
	}
	if ve.Line != 2 {
		t.Errorf("error line = %d, want 2", ve.Line)
	}
}

// TestExamples verifies every program in examples/: the invalid_* programs must
// be rejected and all others accepted.
func TestExamples(t *testing.T) {
	paths, err := filepath.Glob("../../examples/*.arc")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no examples found")
	}
	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			prog, err := parser.Parse(string(src))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, verr := Verify(prog)
			invalid := strings.HasPrefix(name, "invalid_")
			if invalid && verr == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
			if !invalid && verr != nil {
				t.Fatalf("expected %s to verify, got %v", name, verr)
			}
		})
	}
}
