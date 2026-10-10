package verifier

import (
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
    r2 = ARENA_ALLOC r1, 128
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
				"    r2 = ARENA_ALLOC r1, 32\n" +
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
				"    r2 = ARENA_ALLOC r1, 16\n" +
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
				"    r2 = ARENA_ALLOC r1, 64\n" +
				"    r3 = 100\n" +
				"    STORE [r2 + r3], r3\n" +
				"    RET\n",
			"out-of-bounds",
		},
		{
			"allocation exceeds arena",
			"entry:\n" +
				"    r1 = ARENA_CREATE DRAM_LOCAL, 8\n" +
				"    r2 = ARENA_ALLOC r1, 64\n" +
				"    RET\n",
			"exceeds arena capacity",
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
		"    r2 = ARENA_ALLOC r1, 64\n" +
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
		"    r2 = ARENA_ALLOC r1, 64\n" +
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
		"func add(r1: scalar, r2: scalar):\n" +
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
		"func fact(r1: scalar):\n" +
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
		"    r2 = ARENA_ALLOC r5, 64\n" +
		"    r1 = r2\n" +
		"    CALL read\n" +
		"    ARENA_DESTROY r5\n" +
		"    RET r0\n" +
		"func read(r1: ptr(64)):\n" +
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
				"func f(r1: scalar):\n" +
				"    r0 = r1\n" +
				"    RET r0\n",
			"expected scalar",
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
				"    r2 = ARENA_ALLOC r5, 16\n" +
				"    r1 = r2\n" +
				"    CALL f\n" +
				"    RET r0\n" +
				"func f(r1: ptr(64)):\n" +
				"    r0 = LOAD [r1]\n" +
				"    RET r0\n",
			"smaller than the declared ptr(64)",
		},
		{
			"out of bounds through pointer parameter",
			"func main:\n" +
				"    r5 = ARENA_CREATE DRAM_LOCAL, 64\n" +
				"    r2 = ARENA_ALLOC r5, 64\n" +
				"    r1 = r2\n" +
				"    CALL f\n" +
				"    ARENA_DESTROY r5\n" +
				"    RET r0\n" +
				"func f(r1: ptr(16)):\n" +
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
		"func make(r1: arena) -> ptr(64):\n" +
		"    r0 = ARENA_ALLOC r1, 64\n" +
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
				"func f() -> ptr(64):\n    r0 = 5\n    RET r0\n",
			"declares return type ptr",
		},
		{
			"returned allocation too small",
			"func main:\n    r5 = ARENA_CREATE DRAM_LOCAL, 16\n    r1 = r5\n    CALL f\n    ARENA_DESTROY r5\n    r1 = 0\n    RET r1\n" +
				"func f(r1: arena) -> ptr(64):\n    r0 = ARENA_ALLOC r1, 16\n    RET r0\n",
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
				"func f(r1: own arena) -> ptr(64):\n    r0 = ARENA_ALLOC r1, 64\n    ARENA_DESTROY r1\n    RET r0\n",
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
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r1, 64\n    r3 = 7\n    CALL consume\n    STORE [r2], r3\n    RET\n" +
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
	// A stored pointer comes back as a data pointer.
	valid := "func main:\n" +
		"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
		"    r2 = ARENA_ALLOC r1, 128\n" +
		"    r3 = ARENA_ALLOC r1, 8\n" +
		"    r4 = 99\n" +
		"    STORE [r3], r4\n" +
		"    STORE [r2], r3\n" +
		"    r5 = LOAD [r2]\n" +
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
			"store arena in memory",
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 256\n    r2 = ARENA_ALLOC r1, 128\n    STORE [r2], r1\n    ARENA_DESTROY r1\n    RET\n",
			"cannot store a value of type arena",
		},
		{
			"store pointer at dynamic offset",
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 256\n    r2 = ARENA_ALLOC r1, 128\n    r3 = ARENA_ALLOC r1, 8\n    r4 = 0\n    STORE [r2 + r4*8], r3\n    ARENA_DESTROY r1\n    RET\n",
			"only scalars may be stored",
		},
		{
			"untyped load is scalar",
			"func main:\n    r1 = ARENA_CREATE DRAM_LOCAL, 256\n    r2 = ARENA_ALLOC r1, 128\n    r5 = LOAD [r2]\n    r6 = LOAD [r5]\n    ARENA_DESTROY r1\n    RET\n",
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
