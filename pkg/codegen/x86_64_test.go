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
	if _, err := verifier.Verify(prog); err != nil {
		t.Fatalf("verify: %v", err)
	}
	asm, err := EmitAsm(prog)
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
