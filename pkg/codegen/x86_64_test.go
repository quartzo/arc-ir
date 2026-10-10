package codegen

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"arc-ir/pkg/parser"
	"arc-ir/pkg/runtime"
	"arc-ir/pkg/verifier"
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
	// The first four used registers (r1..r4) map to rbx, r12, r13, r14.
	asm := buildAsm(t, loopSrc)
	if !strings.Contains(asm, "call arc_arena_create") {
		t.Fatal("missing call")
	}
	// The index*scale store must use an x86 addressing mode.
	if !strings.Contains(asm, ",8)") {
		t.Errorf("expected a scaled addressing mode in:\n%s", asm)
	}
}

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
		{"call", "func main:\n" +
			"    r1 = 3\n    r2 = 4\n    CALL add\n    PRINT r0\n    RET r0\n" +
			"func add(r1: scalar, r2: scalar):\n" +
			"    r0 = ADD r1, r2\n    RET r0\n", "7"},
		{"fact", "func main:\n" +
			"    r1 = 5\n    CALL fact\n    PRINT r0\n    RET r0\n" +
			"func fact(r1: scalar):\n" +
			"    JGT r1, 1, rec\n    r0 = 1\n    RET r0\n" +
			"rec:\n    r2 = SUB r1, 1\n    r3 = r1\n    r1 = r2\n    CALL fact\n" +
			"    r4 = MUL r3, r0\n    r0 = r4\n    RET r0\n", "120"},
		{"ptr", "func main:\n" +
			"    r5 = ARENA_CREATE DRAM_LOCAL, 64\n    r2 = ARENA_ALLOC r5, 64\n" +
			"    r3 = 42\n    STORE [r2], r3\n    r1 = r2\n    CALL read\n" +
			"    PRINT r0\n    ARENA_DESTROY r5\n    RET r0\n" +
			"func read(r1: ptr(64)):\n    r0 = LOAD [r1]\n    RET r0\n", "42"},
		{"ret_ptr", "func main:\n" +
			"    CALL get_arena\n    r1 = r0\n    CALL make\n    r2 = r0\n" +
			"    r3 = 99\n    STORE [r2], r3\n    r4 = LOAD [r2]\n    PRINT r4\n" +
			"    ARENA_DESTROY r1\n    RET r4\n" +
			"func get_arena() -> arena:\n    r0 = ARENA_CREATE DRAM_LOCAL, 64\n    RET r0\n" +
			"func make(r1: arena) -> ptr(64):\n    r0 = ARENA_ALLOC r1, 64\n    RET r0\n", "99"},
		{"typed_mem", "func main:\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n    r2 = ARENA_ALLOC r1, 128\n" +
			"    r3 = ARENA_ALLOC r1, 8\n    r4 = 99\n    STORE [r3], r4\n    STORE [r2], r3\n" +
			"    r5 = LOAD [r2]\n    r6 = LOAD [r5]\n    PRINT r6\n    ARENA_DESTROY r1\n    RET r6\n", "99"},
		{"growable", "func main:\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, growable(64)\n    r2 = ARENA_ALLOC r1, 64\n" +
			"    r3 = ARENA_ALLOC r1, 64\n    r4 = 77\n    STORE [r3], r4\n    r5 = LOAD [r3]\n" +
			"    PRINT r5\n    ARENA_DESTROY r1\n    RET r5\n", "77"},
		{"float", "func main:\n" +
			"    v1 = 2.5\n    v2 = 4.0\n    v3 = FMUL v1, v2\n    v4 = FADD v3, v1\n" +
			"    PRINTF v4\n    r1 = FTOI v4\n    PRINT r1\n    RET r1\n", "12.5\n12"},
		{"f32", "func main:\n" +
			"    v1 = f32(2.5)\n    v2 = f32(4.0)\n    v3 = FMUL32 v1, v2\n    v4 = FADD32 v3, v1\n" +
			"    PRINTF32 v4\n    r1 = F32TOI v4\n    PRINT r1\n    RET r1\n", "12.5\n12"},
		{"simd", "func main:\n" +
			"    v1 = f32x4(1.0, 2.0, 3.0, 4.0)\n    v2 = f32x4(10.0, 20.0, 30.0, 40.0)\n" +
			"    v3 = VADD.f32x4 v1, v2\n    PRINTV.f32x4 v3\n    RET\n", "11\n22\n33\n44"},
		{"simd64", "func main:\n" +
			"    v1 = f64x2(1.5, 2.5)\n    v2 = f64x2(10.0, 20.0)\n    v3 = VADD.f64x2 v1, v2\n    PRINTV.f64x2 v3\n" +
			"    v4 = i64x2(100, 200)\n    v5 = i64x2(1, 2)\n    v6 = VADD.i64x2 v4, v5\n    PRINTV.i64x2 v6\n    RET\n", "11.5\n22.5\n101\n202"},
		{"struct", "struct Node { next: ptr(8), val: scalar }\n" +
			"func main:\n" +
			"    r1 = ARENA_CREATE DRAM_LOCAL, 256\n    r2 = ARENA_ALLOC r1, Node\n" +
			"    r3 = ARENA_ALLOC r1, 8\n    r4 = 42\n    STORE [r3], r4\n" +
			"    r2.next = r3\n    r5 = 7\n    r2.val = r5\n    r6 = r2.next\n" +
			"    r7 = LOAD [r6]\n    PRINT r7\n    r8 = r2.val\n    PRINT r8\n" +
			"    ARENA_DESTROY r1\n    RET r7\n", "42\n7"},
		{"ptr_struct", "struct Node { next: ptr(8), val: scalar }\n" +
			"func main:\n" +
			"    r9 = ARENA_CREATE DRAM_LOCAL, 256\n    r2 = ARENA_ALLOC r9, Node\n" +
			"    r3 = ARENA_ALLOC r9, 8\n    r4 = 42\n    STORE [r3], r4\n" +
			"    r2.next = r3\n    r5 = 7\n    r2.val = r5\n    r1 = r2\n    CALL read\n" +
			"    PRINT r0\n    ARENA_DESTROY r9\n    RET r0\n" +
			"func read(r1: ptr(Node)):\n    r2 = r1.next\n    r0 = LOAD [r2]\n    RET r0\n", "42"},
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
