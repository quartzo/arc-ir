package codegen

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quartzo/arc-ir/pkg/parser"
	"github.com/quartzo/arc-ir/pkg/runtime"
	"github.com/quartzo/arc-ir/pkg/verifier"
)

func buildAsm(t *testing.T, src string) string {
	t.Helper()
	prog, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res, err := verifier.Verify(prog)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	asm, err := EmitAsm(res.Module)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return asm
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

func TestEmitAsmContains(t *testing.T) {
	asm := buildAsm(t, loopSrc)
	wants := []string{
		".globl main",
		"main:",
		"call arc_arena_create",
		"call arc_arena_alloc",
		"call arc_arena_destroy",
		"call arc_print",
		".L_main_loop:",
		".L_main_done:",
		"jge .L_main_done",
		"jmp .L_main_loop",
		".L_epilogue_main:",
		"ret",
	}
	for _, w := range wants {
		if !strings.Contains(asm, w) {
			t.Errorf("assembly missing %q\n---\n%s", w, asm)
		}
	}
}

func TestEmitAsmRegisterMapping(t *testing.T) {
	// The first used general-purpose registers map to the scratch physical
	// registers (rcx, rdx, rsi, rdi, r8).
	asm := buildAsm(t, loopSrc)
	if !strings.Contains(asm, "call arc_arena_create") {
		t.Fatal("missing call")
	}
	// The index*scale store must use an x86 addressing mode.
	if !strings.Contains(asm, ",8)") {
		t.Errorf("expected a scaled addressing mode in:\n%s", asm)
	}
}

// TestEmitStackArena checks that a stack arena is lowered inline: it never
// calls the runtime allocator and is never destroyed.
func TestEmitStackArena(t *testing.T) {
	asm := buildAsm(t, "func main -> i64\n"+
		"    r1 = ARENA_CREATE STACK, 64\n"+
		"    r2 = ARENA_ALLOC r1, [i64; 8]\n"+
		"    RET\n"+
		"endfunc\n")
	for _, bad := range []string{"arc_arena_create", "arc_arena_alloc", "arc_arena_destroy"} {
		if strings.Contains(asm, bad) {
			t.Errorf("stack arena lowered to runtime call %q:\n%s", bad, asm)
		}
	}
	if !strings.Contains(asm, "(%rsp)") {
		t.Errorf("expected a frame-relative access in:\n%s", asm)
	}
}

// TestInlineNotAppliedWithPreserved checks that a callee which uses a preserved
// register is left as a real call: inlining it would clobber the caller's
// preserved values.
func TestInlineNotAppliedWithPreserved(t *testing.T) {
	asm := buildAsm(t, "inline func f(r1: i64) -> i64\n"+
		"    s0 = ADD r1, 1\n"+
		"    r0 = s0\n"+
		"    RET r0\n"+
		"endfunc\n"+
		"func main -> i64\n"+
		"    r1 = 5\n"+
		"    CALL f\n"+
		"    PRINT r0\n"+
		"    RET r0\n"+
		"endfunc\n")
	if !strings.Contains(asm, "call .Lfunc_f") {
		t.Errorf("expected a real call to f, got:\n%s", asm)
	}
}

// TestEmitAsmArea checks that a module using more general-purpose registers than
// there are physical ones spills the rest to the register area, addressed
// through the reserved base register.
func TestEmitAsmArea(t *testing.T) {
	asm := buildAsm(t, areaScratchSrc)
	if !strings.Contains(asm, areaSymbol+":") {
		t.Errorf("register area %q not declared in:\n%s", areaSymbol, asm)
	}
	if !strings.Contains(asm, "(%rbp)") {
		t.Errorf("no area access through the base register in:\n%s", asm)
	}
}

const areaScratchSrc = "\n" +
	"func main -> i64\n" +
	"    r1 = 1\n" +
	"    r2 = 2\n" +
	"    r3 = 3\n" +
	"    r4 = 4\n" +
	"    r5 = 5\n" +
	"    r6 = 6\n" +
	"    r7 = 7\n" +
	"    r8 = 8\n" +
	"    r9 = 9\n" +
	"    r10 = 10\n" +
	"    r1 = ADD r1, r2\n" +
	"    r1 = ADD r1, r3\n" +
	"    r1 = ADD r1, r4\n" +
	"    r1 = ADD r1, r5\n" +
	"    r1 = ADD r1, r6\n" +
	"    r1 = ADD r1, r7\n" +
	"    r1 = ADD r1, r8\n" +
	"    r1 = ADD r1, r9\n" +
	"    r1 = ADD r1, r10\n" +
	"    PRINT r1\n" +
	"    RET r1\n" +
	"endfunc\n"

// TestCompileAndRun assembles and runs the emitted code with the system C
// compiler (which also assembles .s and links the runtime).
func TestCompileAndRun(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler/assembler available")
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"loop", loopSrc, "9"},
		{"hello", "entry:\n    r1 = 40\n    r2 = 2\n    r3 = ADD r1, r2\n    PRINT r3\n    RET r3\n", "42"},
		{"call", "func main -> i64\n" +
			"    r1 = 3\n" +
			"    r2 = 4\n" +
			"    CALL add\n" +
			"    PRINT r0\n" +
			"    RET r0\n" +
			"endfunc\n" +
			"func add(r1: i64, r2: i64) -> i64\n" +
			"    r0 = ADD r1, r2\n" +
			"    RET r0\n" +
			"endfunc\n", "7"},
		{"fact", "func main -> i64\n" +
			"    r1 = 5\n" +
			"    CALL fact\n" +
			"    PRINT r0\n" +
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
			"endfunc\n", "120"},
		{"ptr", "func main -> i64\n" +
			"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC s0, [i64; 8]\n" +
			"    r3 = 42\n" +
			"    STORE [r2], r3\n" +
			"    r1 = r2\n" +
			"    CALL read\n" +
			"    PRINT r0\n" +
			"    RET r0\n" +
			"endfunc\n" +
			"func read(r1: ptr([i64; 8])) -> i64\n" +
			"    r0 = LOAD [r1]\n" +
			"    RET r0\n" +
			"endfunc\n", "42"},
		{"ret_ptr", "func main -> i64\n" +
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
			"func make(r1: arena) -> r1: ptr([i64; 8])\n" +
			"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
			"    RET r0\n" +
			"endfunc\n", "99"},
		{"typed_mem", "struct Rec\n" +
			"    a: u8\n" +
			"    b: u16\n" +
			"    c: u32\n" +
			"    d: u64\n" +
			"endstruct\n" +
			"func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n" +
			"    r2 = ARENA_ALLOC r1, Rec\n" +
			"    r3 = 200\n" +
			"    r2.a = r3\n" +
			"    r4 = 40000\n" +
			"    r2.b = r4\n" +
			"    r5 = 3000000000\n" +
			"    r2.c = r5\n" +
			"    r6 = 123456789\n" +
			"    r2.d = r6\n" +
			"    r7 = r2.a\n" +
			"    PRINT r7\n" +
			"    r8 = r2.b\n" +
			"    PRINT r8\n" +
			"    r9 = r2.c\n" +
			"    PRINT r9\n" +
			"    r10 = r2.d\n" +
			"    PRINT r10\n" +
			"    RET r7\n" +
			"endfunc\n", "200\n40000\n3000000000\n123456789"},
		{"growable", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, growable(64)\n" +
			"    r2 = ARENA_ALLOC r1, [i64; 8]\n" +
			"    r3 = ARENA_ALLOC r1, [i64; 8]\n" +
			"    r4 = 77\n" +
			"    STORE [r3], r4\n" +
			"    r5 = LOAD [r3]\n" +
			"    PRINT r5\n" +
			"    RET r5\n" +
			"endfunc\n", "77"},
		{"auto_free", "func main -> i64\n" +
			"    s0 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r1 = ARENA_ALLOC s0, [i64; 8]\n" +
			"    r2 = 42\n" +
			"    STORE [r1], r2\n" +
			"    r3 = LOAD [r1]\n" +
			"    PRINT r3\n" +
			"    RET r3\n" +
			"endfunc\n", "42"},
		{"stack_arena", "func main -> i64\n" +
			"    r1 = ARENA_CREATE STACK, 128\n" +
			"    r2 = ARENA_ALLOC r1, [i64; 16]\n" +
			"    r3 = 0\n" +
			"loop:\n" +
			"    JGE r3, 10, done\n" +
			"    STORE [r2 + r3*8], r3\n" +
			"    r3 = ADD r3, 1\n" +
			"    JMP loop\n" +
			"done:\n" +
			"    r4 = LOAD [r2 + r3*8 - 8]\n" +
			"    PRINT r4\n" +
			"    RET r4\n" +
			"endfunc\n", "9"},
		{"stack_two_arenas", "func main -> i64\n" +
			"    s0 = ARENA_CREATE STACK, 64\n" +
			"    r1 = ARENA_ALLOC s0, [i64; 8]\n" +
			"    s1 = ARENA_CREATE STACK, 64\n" +
			"    r2 = ARENA_ALLOC s1, [i64; 8]\n" +
			"    r3 = 5\n" +
			"    STORE [r1], r3\n" +
			"    r4 = 6\n" +
			"    STORE [r2], r4\n" +
			"    r5 = LOAD [r1]\n" +
			"    r6 = LOAD [r2]\n" +
			"    r7 = ADD r5, r6\n" +
			"    PRINT r7\n" +
			"    RET r7\n" +
			"endfunc\n", "11"},
		{"stack_param", "func main -> i64\n" +
			"    s0 = ARENA_CREATE STACK, 64\n" +
			"    r1 = s0\n" +
			"    CALL fill\n" +
			"    r2 = LOAD [r0]\n" +
			"    PRINT r2\n" +
			"    RET r2\n" +
			"endfunc\n" +
			"func fill(r1: stack(64)) -> r1: ptr([i64; 8])\n" +
			"    r0 = ARENA_ALLOC r1, [i64; 8]\n" +
			"    r3 = 77\n" +
			"    STORE [r0], r3\n" +
			"    RET r0\n" +
			"endfunc\n", "77"},
		{"inline", "inline func add1(r1: i64) -> i64\n" +
			"    r2 = ADD r1, 1\n" +
			"    r0 = r2\n" +
			"    RET r0\n" +
			"endfunc\n" +
			"func main -> i64\n" +
			"    s0 = 0\n" +
			"    r0 = 0\n" +
			"loop:\n" +
			"    JGE s0, 5, done\n" +
			"    r1 = s0\n" +
			"    CALL add1\n" +
			"    s0 = ADD s0, 1\n" +
			"    JMP loop\n" +
			"done:\n" +
			"    PRINT r0\n" +
			"    RET r0\n" +
			"endfunc\n", "5"},
		{"float", "func main -> i64\n" +
			"    v1 = 2.5\n" +
			"    v2 = 4.0\n" +
			"    v3 = FMUL v1, v2\n" +
			"    v4 = FADD v3, v1\n" +
			"    PRINTF v4\n" +
			"    r1 = FTOI v4\n" +
			"    PRINT r1\n" +
			"    RET r1\n" +
			"endfunc\n", "12.5\n12"},
		{"f32", "func main -> i64\n" +
			"    v1 = f32(2.5)\n" +
			"    v2 = f32(4.0)\n" +
			"    v3 = FMUL v1, v2\n" +
			"    v4 = FADD v3, v1\n" +
			"    PRINTF v4\n" +
			"    r1 = FTOI v4\n" +
			"    PRINT r1\n" +
			"    RET r1\n" +
			"endfunc\n", "12.5\n12"},
		{"simd", "func main -> i64\n" +
			"    v1 = f32x4(1.0, 2.0, 3.0, 4.0)\n" +
			"    v2 = f32x4(10.0, 20.0, 30.0, 40.0)\n" +
			"    v3 = VADD v1, v2\n" +
			"    PRINTV v3\n" +
			"    RET\n" +
			"endfunc\n", "11\n22\n33\n44"},
		{"simd64", "func main -> i64\n" +
			"    v1 = f64x2(1.5, 2.5)\n" +
			"    v2 = f64x2(10.0, 20.0)\n" +
			"    v3 = VADD v1, v2\n" +
			"    PRINTV v3\n" +
			"    v4 = i64x2(100, 200)\n" +
			"    v5 = i64x2(1, 2)\n" +
			"    v6 = VADD v4, v5\n" +
			"    PRINTV v6\n" +
			"    RET\n" +
			"endfunc\n", "11.5\n22.5\n101\n202"},
		{"struct", "struct Node\n" +
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
			"    r8 = r2.val\n" +
			"    PRINT r8\n" +
			"    RET r7\n" +
			"endfunc\n", "42\n7"},
		{"ptr_struct", "struct Node\n" +
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
			"endfunc\n", "42"},
		{"array", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 128\n" +
			"    r2 = ARENA_ALLOC r1, [i64; 4]\n" +
			"    r3 = 11\n" +
			"    STORE [r2], r3\n" +
			"    r4 = 22\n" +
			"    STORE [r2 + 8], r4\n" +
			"    r5 = LOAD [r2]\n" +
			"    PRINT r5\n" +
			"    r6 = LOAD [r2 + 8]\n" +
			"    PRINT r6\n" +
			"    RET r5\n" +
			"endfunc\n", "11\n22"},
		{"f64_array", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 128\n" +
			"    r2 = ARENA_ALLOC r1, [f64; 4]\n" +
			"    v1 = 1.5\n" +
			"    STORE [r2 + 8], v1\n" +
			"    v2 = LOAD [r2 + 8]\n" +
			"    PRINTF v2\n" +
			"    RET\n" +
			"endfunc\n", "1.5"},
		{"bytes", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [u8; 8]\n" +
			"    r3 = 65\n" +
			"    STORE [r2], r3\n" +
			"    r4 = 66\n" +
			"    STORE [r2 + 1], r4\n" +
			"    r5 = LOAD [r2]\n" +
			"    PRINT r5\n" +
			"    r6 = LOAD [r2 + 1]\n" +
			"    PRINT r6\n" +
			"    RET r5\n" +
			"endfunc\n", "65\n66"},
		{"u16_i8", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [u16; 2]\n" +
			"    r3 = 50000\n" +
			"    STORE [r2], r3\n" +
			"    r4 = LOAD [r2]\n" +
			"    PRINT r4\n" +
			"    r5 = ARENA_ALLOC r1, [i8; 1]\n" +
			"    r6 = -5\n" +
			"    STORE [r5], r6\n" +
			"    r7 = LOAD [r5]\n" +
			"    PRINT r7\n" +
			"    RET r4\n" +
			"endfunc\n", "50000\n-5"},
		{"unsigned_cmp", "func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, [u8; 2]\n" +
			"    r3 = 200\n" +
			"    STORE [r2], r3\n" +
			"    r4 = LOAD [r2]\n" +
			"    r0 = 0\n" +
			"    JGT r4, 100, big\n" +
			"    r0 = 1\n" +
			"big:\n" +
			"    PRINT r0\n" +
			"    RET r0\n" +
			"endfunc\n", "0"},
		{"packed", "packed struct P\n" +
			"    a: u8\n" +
			"    b: u32\n" +
			"endstruct\n" +
			"func main -> i64\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 64\n" +
			"    r2 = ARENA_ALLOC r1, P\n" +
			"    r3 = 9\n" +
			"    r2.a = r3\n" +
			"    r4 = 123456\n" +
			"    r2.b = r4\n" +
			"    r5 = r2.b\n" +
			"    PRINT r5\n" +
			"    RET r5\n" +
			"endfunc\n", "123456"},
		{"enum", "enum Value\n" +
			"    Int(n: i64)\n" +
			"    Pair(a: i32, b: i32)\n" +
			"    Nil\n" +
			"endenum\n" +
			"func main -> i64\n" +
			"    s0 = ARENA_CREATE DRAM_LOCAL, 256\n" +
			"    r2 = ARENA_ALLOC s0, Value\n" +
			"    VARIANT r2, Value.Int\n" +
			"    r3 = 21\n" +
			"    r2.n = r3\n" +
			"    r1 = r2\n" +
			"    CALL twice\n" +
			"    PRINT r0\n" +
			"    RET\n" +
			"endfunc\n" +
			"func twice(r1: ptr(Value)) -> i64\n" +
			"    JV r1, Value.Int, as_int\n" +
			"    JV r1, Value.Pair, as_pair\n" +
			"    r0 = 0\n" +
			"    RET r0\n" +
			"as_int:\n" +
			"    r0 = r1.n\n" +
			"    r0 = ADD r0, r0\n" +
			"    RET r0\n" +
			"as_pair:\n" +
			"    r2 = r1.a\n" +
			"    r3 = r1.b\n" +
			"    r0 = ADD r2, r3\n" +
			"    RET r0\n" +
			"endfunc\n", "42"},
		{"preserved_block", "func main -> i64\n" +
			"    s0 = 42\n" +
			"    CALL leaf\n" +
			"    PRINT s0\n" +
			"    RET\n" +
			"endfunc\n" +
			"func leaf -> i64\n" +
			"    s0 = 7\n" +
			"    RET\n" +
			"endfunc\n", "42"},
		{"area_scratch", areaScratchSrc, "55"},
		{"area_preserved", "func main -> i64\n" +
			"    s0 = 10\n" +
			"    s1 = 11\n" +
			"    s2 = 12\n" +
			"    s3 = 13\n" +
			"    s4 = 14\n" +
			"    s5 = 15\n" +
			"    s6 = 16\n" +
			"    s7 = 17\n" +
			"    s8 = 18\n" +
			"    CALL leaf\n" +
			"    PRINT s5\n" +
			"    PRINT s6\n" +
			"    PRINT s7\n" +
			"    PRINT s8\n" +
			"    PRINT s0\n" +
			"    RET\n" +
			"endfunc\n" +
			"func leaf -> i64\n" +
			"    s5 = 999\n" +
			"    s6 = 999\n" +
			"    s7 = 999\n" +
			"    s8 = 999\n" +
			"    RET\n" +
			"endfunc\n",
			"15\n16\n17\n18\n10"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asm := buildAsm(t, tc.src)
			dir := t.TempDir()
			asmPath := filepath.Join(dir, "prog.s")
			rtPath := filepath.Join(dir, runtime.SourceName)
			if err := os.WriteFile(asmPath, []byte(asm), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rtPath, []byte(runtime.Source), 0o644); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "prog")
			if out, err := exec.Command(cc, "-O2", "-o", bin, asmPath, rtPath).CombinedOutput(); err != nil {
				t.Fatalf("cc failed: %v\n%s", err, out)
			}

			out, err := exec.Command(bin).CombinedOutput()
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("run failed: %v\n%s", err, out)
				}
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEmitAsmLivenessSpill checks that a call spills only the registers live
// across it, and that the prologue pushes only the callee-saved registers the
// function writes. The loop uses only scratch registers, so no callee-saved
// register other than rbp (the area base) is pushed, and the registers dead
// across every call (r2, r3) are never spilled.
func TestEmitAsmLivenessSpill(t *testing.T) {
	asm := buildAsm(t, loopSrc)
	for _, bad := range []string{"pushq %rbx", "pushq %r12", "pushq %r13", "pushq %r14", "pushq %r15"} {
		if strings.Contains(asm, bad) {
			t.Errorf("prologue pushes an unused callee-saved register (%q):\n%s", bad, asm)
		}
	}
	// r2 is %rdx (area slot 16), r3 is %rsi (area slot 24); both are dead across
	// every call, so neither is ever spilled.
	for _, bad := range []string{"16(%rbp)", "24(%rbp)"} {
		if strings.Contains(asm, bad) {
			t.Errorf("a register dead across every call was spilled (%q):\n%s", bad, asm)
		}
	}
}

// ffiHelpers are C functions the extern tests link against, exercising integer,
// six-argument, mixed integer/float and pointer (string) calls.
const ffiHelpers = `
long ffi_add3(long a, long b, long c) { return a + b + c; }
long ffi_sum6(long a, long b, long c, long d, long e, long f) {
    return a + b + c + d + e + f;
}
double ffi_mix(long a, double b) { return (double)a + b; }
long ffi_strlen(const char *s) { long n = 0; while (s[n]) n++; return n; }
`

// TestCompileAndRunExtern compiles and runs programs that call foreign (C)
// functions through the System V AMD64 ABI.
func TestCompileAndRunExtern(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler/assembler available")
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"int", "extern \"ffi_add3\" func add3(r1: i64, r2: i64, r3: i64) -> i64\n" +
			"func main -> i64\n" +
			"    r1 = 10\n" +
			"    r2 = 20\n" +
			"    r3 = 12\n" +
			"    CALL add3\n" +
			"    PRINT r0\n" +
			"    RET r0\n" +
			"endfunc\n", "42"},
		{"six_args", "extern \"ffi_sum6\" func sum6(r1: i64, r2: i64, r3: i64, r4: i64, r5: i64, r6: i64) -> i64\n" +
			"func main -> i64\n" +
			"    r1 = 1\n" +
			"    r2 = 2\n" +
			"    r3 = 3\n" +
			"    r4 = 4\n" +
			"    r5 = 5\n" +
			"    r6 = 6\n" +
			"    CALL sum6\n" +
			"    PRINT r0\n" +
			"    RET r0\n" +
			"endfunc\n", "21"},
		{"mixed_float", "extern \"ffi_mix\" func mix(r1: i64, v0: f64) -> f64\n" +
			"func main -> i64\n" +
			"    r1 = 3\n" +
			"    v0 = 2.5\n" +
			"    CALL mix\n" +
			"    PRINTF v0\n" +
			"    RET\n" +
			"endfunc\n", "5.5"},
		{"ptr_string", "extern \"ffi_strlen\" func strlen(r1: ptr([u8; 16])) -> i64\n" +
			"func main -> i64\n" +
			"    s0 = ARENA_CREATE DRAM_LOCAL, 16\n" +
			"    r1 = ARENA_ALLOC s0, [u8; 16]\n" +
			"    r2 = 72\n" +
			"    STORE [r1], r2\n" +
			"    r3 = 105\n" +
			"    STORE [r1 + 1], r3\n" +
			"    r4 = 0\n" +
			"    STORE [r1 + 2], r4\n" +
			"    CALL strlen\n" +
			"    PRINT r0\n" +
			"    RET r0\n" +
			"endfunc\n", "2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asm := buildAsm(t, tc.src)
			dir := t.TempDir()
			asmPath := filepath.Join(dir, "prog.s")
			rtPath := filepath.Join(dir, runtime.SourceName)
			helpPath := filepath.Join(dir, "ffi_helpers.c")
			if err := os.WriteFile(asmPath, []byte(asm), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rtPath, []byte(runtime.Source), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(helpPath, []byte(ffiHelpers), 0o644); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "prog")
			if out, err := exec.Command(cc, "-O2", "-o", bin, asmPath, rtPath, helpPath).CombinedOutput(); err != nil {
				t.Fatalf("cc failed: %v\n%s", err, out)
			}

			out, err := exec.Command(bin).CombinedOutput()
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("run failed: %v\n%s", err, out)
				}
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}
