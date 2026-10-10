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
    ARENA_DESTROY r1
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
			"use after destroy",
			"entry:\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
				"    r2 = ARENA_ALLOC r1, [i64; 4]\n" +
				"    r3 = 7\n" +
				"    ARENA_DESTROY r1\n" +
				"    STORE [r2], r3\n" +
				"    RET\n",
			"use-after-destroy",
		},
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
		{
			"double destroy",
			"entry:\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 8\n" +
				"    ARENA_DESTROY r1\n" +
				"    ARENA_DESTROY r1\n" +
				"    RET\n",
			"already destroyed",
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
		"    ARENA_DESTROY r1\n" +
		"    RET\n" +
		"out:\n" +
		"    ARENA_DESTROY r1\n" +
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
	src := "func main:\n" +
		"    r1 = 3\n" +
		"    r2 = 4\n" +
		"    CALL add\n" +
		"    PRINT r0\n" +
		"    RET r0\n" +
		"func add(r1: i64, r2: i64):\n" +
		"    r0 = ADD r1, r2\n" +
		"    RET r0\n"
	res, err := Verify(mustParse(t, src))
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if res.Funcs != 2 {
		t.Errorf("funcs = %d, want 2", res.Funcs)
	}
}

func TestVerifyRecursion(t *testing.T) {
	src := "func main:\n" +
		"    r1 = 5\n" +
		"    CALL fact\n" +
		"    RET r0\n" +
		"func fact(r1: i64):\n" +
		"    JGT r1, 1, rec\n" +
		"    r0 = 1\n" +
		"    RET r0\n" +
		"rec:\n" +
		"    r2 = SUB r1, 1\n" +
		"    r3 = r1\n" +
		"    r1 = r2\n" +
		"    CALL fact\n" +
		"    r4 = MUL r3, r0\n" +
		"    r0 = r4\n" +
		"    RET r0\n"
	if _, err := Verify(mustParse(t, src)); err != nil {
		t.Fatalf("expected recursion to verify, got %v", err)
	}
}

func TestVerifyPointerParam(t *testing.T) {
	src := "func main:\n" +
		"    r5 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r2 = ARENA_ALLOC r5, [i64; 8]\n" +
		"    r1 = r2\n" +
		"    CALL read\n" +
		"    ARENA_DESTROY r5\n" +
		"    RET r0\n" +
		"func read(r1: ptr([i64; 8])):\n" +
		"    r0 = LOAD [r1]\n" +
		"    RET r0\n"
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
			"func main:\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    CALL f\n" +
				"    RET\n" +
				"func f(r1: i64):\n" +
				"    r0 = r1\n" +
				"    RET r0\n",
			"expected an integer",
		},
		{
			"unknown function",
			"func main:\n" +
				"    CALL nope\n" +
				"    RET\n",
			"undefined function",
		},
		{
			"pointer argument too small",
			"func main:\n" +
				"    r5 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    r2 = ARENA_ALLOC r5, [i64; 2]\n" +
				"    r1 = r2\n" +
				"    CALL f\n" +
				"    RET r0\n" +
				"func f(r1: ptr([i64; 8])):\n" +
				"    r0 = LOAD [r1]\n" +
				"    RET r0\n",
			"smaller than the required",
		},
		{
			"out of bounds through pointer parameter",
			"func main:\n" +
				"    r5 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    r2 = ARENA_ALLOC r5, [i64; 8]\n" +
				"    r1 = r2\n" +
				"    CALL f\n" +
				"    ARENA_DESTROY r5\n" +
				"    RET r0\n" +
				"func f(r1: ptr([i64; 2])):\n" +
				"    r0 = LOAD [r1 + 32]\n" +
				"    RET r0\n",
			"out-of-bounds",
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

func TestVerifyNonScalarReturns(t *testing.T) {
	src := "func main:\n" +
		"    CALL get_arena\n" +
		"    r1 = r0\n" +
		"    CALL make\n" +
		"    r2 = r0\n" +
		"    r3 = 99\n" +
		"    STORE [r2], r3\n" +
		"    r4 = LOAD [r2]\n" +
		"    PRINT r4\n" +
		"    ARENA_DESTROY r1\n" +
		"    RET r4\n" +
		"func get_arena() -> arena:\n" +
		"    r0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    RET r0\n" +
		"func make(r1: arena) -> ptr([i64; 8]):\n" +
		"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET r0\n"
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
			"func main:\n    CALL f\n    r1 = 0\n    RET r1\n" +
				"func f() -> ptr([i64; 8]):\n    r0 = 5\n    RET r0\n",
			"declares return type ptr",
		},
		{
			"returned allocation too small",
			"func main:\n    r5 = ARENA_CREATE DRAM_LOCAL, 16\n    r1 = r5\n    CALL f\n    ARENA_DESTROY r5\n    r1 = 0\n    RET r1\n" +
				"func f(r1: arena) -> ptr([i64; 8]):\n    r0 = ARENA_ALLOC r1, [i64; 2]\n    RET r0\n",
			"16-byte allocation",
		},
		{
			"bare RET for non-scalar",
			"func main:\n    CALL f\n    ARENA_DESTROY r0\n    r1 = 0\n    RET r1\n" +
				"func f() -> arena:\n    RET\n",
			"RET has no value",
		},
		{
			"return into destroyed arena",
			"func main:\n    r5 = ARENA_CREATE DRAM_LOCAL, 64\n    r1 = r5\n    CALL f\n    r1 = 0\n    RET r1\n" +
				"func f(r1: own arena) -> ptr([i64; 8]):\n    r0 = ARENA_ALLOC r1, [i64; 8]\n    ARENA_DESTROY r1\n    RET r0\n",
			"no longer live",
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
	// Transferring ownership and destroying inside the callee is allowed.
	valid := "func main:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    CALL consume\n" +
		"    RET\n" +
		"func consume(r1: own arena):\n" +
		"    ARENA_DESTROY r1\n" +
		"    RET\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected own transfer to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"leak",
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    RET\n",
			"never released",
		},
		{
			"destroy borrowed arena",
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    CALL consume\n    ARENA_DESTROY r1\n    RET\n" +
				"func consume(r1: arena):\n    ARENA_DESTROY r1\n    RET\n",
			"borrowed arena",
		},
		{
			"use after ownership transfer",
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [i64; 8]\n    r3 = 7\n    CALL consume\n    STORE [r2], r3\n    RET\n" +
				"func consume(r1: own arena):\n    ARENA_DESTROY r1\n    RET\n",
			"use-after-destroy",
		},
		{
			"cannot transfer borrowed arena",
			"func main:\n    r5 = ARENA_CREATE DRAM_LOCAL, 64\n    r1 = r5\n    CALL give\n    ARENA_DESTROY r5\n    RET\n" +
				"func give(r1: arena):\n    CALL take\n    RET\n" +
				"func take(r1: own arena):\n    ARENA_DESTROY r1\n    RET\n",
			"cannot transfer a borrowed arena",
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
	valid := "struct Box { p: ptr([i64; 1]) }\n" +
		"func main:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
		"    r2 = ARENA_ALLOC r1, Box\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 1]\n" +
		"    r4 = 99\n" +
		"    STORE [r3], r4\n" +
		"    r2.p = r3\n" +
		"    r5 = r2.p\n" +
		"    r6 = LOAD [r5]\n" +
		"    PRINT r6\n" +
		"    ARENA_DESTROY r1\n" +
		"    RET r6\n"
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
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 256\n    r2 = ARENA_ALLOC r1, [i64; 16]\n    STORE [r2], r1\n    ARENA_DESTROY r1\n    RET\n",
			"array element expects an integer",
		},
		{
			"store pointer in an integer array",
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 256\n    r2 = ARENA_ALLOC r1, [i64; 16]\n    r3 = ARENA_ALLOC r1, [i64; 1]\n    STORE [r2], r3\n    ARENA_DESTROY r1\n    RET\n",
			"array element expects an integer",
		},
		{
			"integer element is not a pointer",
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 256\n    r2 = ARENA_ALLOC r1, [i64; 16]\n    r5 = LOAD [r2]\n    r6 = LOAD [r5]\n    ARENA_DESTROY r1\n    RET\n",
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
	over := "func main:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 128\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r4 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    ARENA_DESTROY r1\n" +
		"    RET\n"
	if _, err := Verify(mustParse(t, over)); err == nil || !strings.Contains(err.Error(), "arena exhausted") {
		t.Fatalf("expected cumulative exhaustion, got %v", err)
	}

	// An exact fit is accepted.
	fits := "func main:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 128\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    ARENA_DESTROY r1\n" +
		"    RET\n"
	if _, err := Verify(mustParse(t, fits)); err != nil {
		t.Fatalf("expected exact fit to verify, got %v", err)
	}

	// Capacity is enforced across a function boundary via arena(<size>).
	across := "func main:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    CALL fill\n" +
		"    ARENA_DESTROY r1\n" +
		"    RET\n" +
		"func fill(r1: arena(64)):\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET\n"
	if _, err := Verify(mustParse(t, across)); err == nil || !strings.Contains(err.Error(), "arena exhausted") {
		t.Fatalf("expected cross-function exhaustion, got %v", err)
	}
}

func FuzzVerify(f *testing.F) {
	f.Add("entry:\n    r1 = 1\n    RET\n")
	f.Add("func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    RET\n")
	f.Add("func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [i64; 8]\n    ARENA_DESTROY r1\n    RET\n")
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
	valid := "func main:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, growable(64)\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    r4 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    ARENA_DESTROY r1\n" +
		"    RET\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected growable to verify, got %v", err)
	}

	// A growable arena satisfies any declared parameter capacity.
	across := "func main:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, growable(64)\n" +
		"    CALL fill\n" +
		"    ARENA_DESTROY r1\n" +
		"    RET\n" +
		"func fill(r1: arena(4096)):\n" +
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
		"    RET\n"
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
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, growable(64)\n    r2 = ARENA_ALLOC r1, [i64; 16]\n    ARENA_DESTROY r1\n    RET\n",
			"exceeds chunk size",
		},
		{
			"fixed arena smaller than declared parameter capacity",
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    CALL fill\n    ARENA_DESTROY r1\n    RET\n" +
				"func fill(r1: arena(4096)):\n    r2 = ARENA_ALLOC r1, [i64; 8]\n    RET\n",
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

func TestVerifyFloat(t *testing.T) {
	valid := "func main:\n" +
		"    v1 = 2.5\n" +
		"    v2 = 4.0\n" +
		"    v3 = FMUL v1, v2\n" +
		"    v4 = FADD v3, v1\n" +
		"    PRINTF v4\n" +
		"    r1 = FTOI v4\n" +
		"    RET r1\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected float program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"float as pointer", "func main:\n    v1 = 1.0\n    r1 = LOAD [v1]\n    RET\n", "expected data pointer"},
		{"FADD on scalar", "func main:\n    r1 = 1\n    v1 = FADD r1, r1\n    RET\n", "expected a float"},
		{"ITOF on float", "func main:\n    v1 = 1.0\n    v2 = ITOF v1\n    RET\n", "expected integer"},
		{"FTOI into vector", "func main:\n    v1 = 1.0\n    v2 = FTOI v1\n    RET\n", "requires a general register"},
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
	valid := "func main:\n" +
		"    v1 = f32(2.5)\n" +
		"    v2 = f32(4.0)\n" +
		"    v3 = FMUL v1, v2\n" +
		"    v4 = FADD v3, v1\n" +
		"    PRINTF v4\n" +
		"    r1 = FTOI v4\n" +
		"    RET r1\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected f32 program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"mixing f32 and f64", "func main:\n    v1 = 1.0\n    v2 = f32(1.0)\n    v3 = FADD v1, v2\n    RET\n", "expected f64"},
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
	valid := "func main:\n" +
		"    v1 = f32x4(1.0, 2.0, 3.0, 4.0)\n" +
		"    v2 = f32x4(10.0, 20.0, 30.0, 40.0)\n" +
		"    v3 = VADD v1, v2\n" +
		"    PRINTV v3\n" +
		"    RET\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected simd program to verify, got %v", err)
	}

	valid64 := "func main:\n" +
		"    v1 = f64x2(1.5, 2.5)\n" +
		"    v2 = f64x2(10.0, 20.0)\n" +
		"    v3 = VADD v1, v2\n" +
		"    PRINTV v3\n" +
		"    RET\n"
	if _, err := Verify(mustParse(t, valid64)); err != nil {
		t.Fatalf("expected f64x2 program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"mixing kinds", "func main:\n    v1 = f32x4(1.0)\n    v2 = i32x4(1)\n    v3 = VADD v1, v2\n    RET\n", "expected f32x4"},
		{"scalar op on vector", "func main:\n    v1 = f32x4(1.0)\n    r1 = ADD v1, v1\n    RET\n", "expected integer"},
		{"packed 64-bit multiply", "func main:\n    v1 = i64x2(1)\n    v2 = VMUL v1, v1\n    RET\n", "not available"},
		{"packed integer divide", "func main:\n    v1 = i32x4(1)\n    v2 = VDIV v1, v1\n    RET\n", "not available"},
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
	valid := "struct Node { next: ptr([i64; 1]), val: i64 }\n" +
		"func main:\n" +
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
		"    ARENA_DESTROY r1\n" +
		"    RET r7\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected struct program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"uninitialized field", "struct N { a: i64 }\nfunc main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, N\n    r3 = r2.a\n    ARENA_DESTROY r1\n    RET\n", "not initialized"},
		{"wrong field type", "struct N { p: ptr([i64; 1]) }\nfunc main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, N\n    r3 = 5\n    r2.p = r3\n    ARENA_DESTROY r1\n    RET\n", "field expects"},
		{"field on non-struct", "func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [i64; 1]\n    r3 = r2.x\n    ARENA_DESTROY r1\n    RET\n", "struct pointer"},
		{"unknown field", "struct N { a: i64 }\nfunc main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, N\n    r3 = r2.b\n    ARENA_DESTROY r1\n    RET\n", "no field"},
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
	valid := "struct Node { next: ptr([i64; 1]), val: i64 }\n" +
		"func main:\n" +
		"    r9 = ARENA_CREATE DRAM_LOCAL, 256\n" +
		"    r2 = ARENA_ALLOC r9, Node\n" +
		"    r3 = ARENA_ALLOC r9, [i64; 1]\n" +
		"    r4 = 42\n" +
		"    STORE [r3], r4\n" +
		"    r2.next = r3\n" +
		"    r5 = 7\n" +
		"    r2.val = r5\n" +
		"    r1 = r2\n" +
		"    CALL read\n" +
		"    PRINT r0\n" +
		"    ARENA_DESTROY r9\n" +
		"    RET r0\n" +
		"func read(r1: ptr(Node)):\n" +
		"    r2 = r1.next\n" +
		"    r0 = LOAD [r2]\n" +
		"    RET r0\n"
	if _, err := Verify(mustParse(t, valid)); err != nil {
		t.Fatalf("expected ptr(Node) program to verify, got %v", err)
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"raw ptr for struct", "struct N { a: i64 }\nfunc main:\n    r9 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r9, [i64; 8]\n    r1 = r2\n    CALL f\n    ARENA_DESTROY r9\n    RET\nfunc f(r1: ptr(N)):\n    RET\n", "expected a N pointer"},
		{"uninitialized struct", "struct N { a: i64 }\nfunc main:\n    r9 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r9, N\n    r1 = r2\n    CALL f\n    ARENA_DESTROY r9\n    RET\nfunc f(r1: ptr(N)):\n    RET\n", "not initialized"},
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
		{"i64 array", "func main:\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [i64; 4]\n" +
			"    r3 = 7\n" +
			"    STORE [r2 + 16], r3\n" +
			"    r4 = LOAD [r2 + 16]\n" +
			"    PRINT r4\n" +
			"    ARENA_DESTROY r1\n" +
			"    RET r4\n"},
		{"f64 array", "func main:\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [f64; 4]\n" +
			"    v1 = 2.5\n" +
			"    STORE [r2 + 8], v1\n" +
			"    v2 = LOAD [r2 + 8]\n" +
			"    PRINTF v2\n" +
			"    ARENA_DESTROY r1\n" +
			"    RET\n"},
		{"array parameter", "func main:\n" +
			"    r5 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r5, [i64; 8]\n" +
			"    r1 = r2\n" +
			"    CALL read\n" +
			"    ARENA_DESTROY r5\n" +
			"    RET r0\n" +
			"func read(r1: ptr([i64; 8])):\n" +
			"    r0 = LOAD [r1]\n" +
			"    RET r0\n"},
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
		{"wrong element type", "func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [f64; 4]\n    r3 = 7\n    STORE [r2], r3\n    ARENA_DESTROY r1\n    RET\n", "array element expects"},
		{"f64 element needs vector register", "func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [f64; 4]\n    r3 = LOAD [r2]\n    ARENA_DESTROY r1\n    RET\n", "requires a vector register"},
		{"i64 element needs general register", "func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [i64; 4]\n    v1 = LOAD [r2]\n    ARENA_DESTROY r1\n    RET\n", "requires a general register"},
		{"array out of bounds", "func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [i64; 4]\n    r3 = LOAD [r2 + 32]\n    ARENA_DESTROY r1\n    RET\n", "out-of-bounds"},
		{"array argument too small", "func main:\n    r5 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r5, [i64; 2]\n    r1 = r2\n    CALL f\n    ARENA_DESTROY r5\n    RET\nfunc f(r1: ptr([i64; 8])):\n    RET\n", "smaller than the required"},
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
		{"u8 array", "func main:\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [u8; 8]\n" +
			"    r3 = 200\n" +
			"    STORE [r2 + 3], r3\n" +
			"    r4 = LOAD [r2 + 3]\n" +
			"    PRINT r4\n" +
			"    ARENA_DESTROY r1\n" +
			"    RET r4\n"},
		{"i8 array sign extends", "func main:\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [i8; 4]\n" +
			"    r3 = 1\n" +
			"    STORE [r2], r3\n" +
			"    r4 = LOAD [r2]\n" +
			"    ARENA_DESTROY r1\n" +
			"    RET r4\n"},
		{"mixed-width struct", "struct R { a: u8, b: u16, c: u32, d: u64 }\n" +
			"func main:\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, R\n" +
			"    r3 = 7\n" +
			"    r2.a = r3\n" +
			"    r2.b = r3\n" +
			"    r2.c = r3\n" +
			"    r2.d = r3\n" +
			"    r4 = r2.c\n" +
			"    ARENA_DESTROY r1\n" +
			"    RET r4\n"},
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
		{"float into integer array", "func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [u8; 8]\n    v1 = 1.5\n    STORE [r2], v1\n    ARENA_DESTROY r1\n    RET\n", "array element expects an integer"},
		{"float element into gp", "func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [f64; 2]\n    r3 = LOAD [r2]\n    ARENA_DESTROY r1\n    RET\n", "requires a vector register"},
		{"out of bounds by width", "func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, [u8; 8]\n    r3 = LOAD [r2 + 8]\n    ARENA_DESTROY r1\n    RET\n", "out-of-bounds"},
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
	src := "func main:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
		"    r2 = ARENA_ALLOC r1, [u8; 2]\n" +
		"    r3 = 200\n" +
		"    STORE [r2], r3\n" +
		"    r4 = LOAD [r2]\n" +
		"    r0 = 0\n" +
		"    JGT r4, 100, big\n" +
		"    r0 = 1\n" +
		"big:\n" +
		"    ARENA_DESTROY r1\n" +
		"    RET r0\n"
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
