package parser

import (
	"strings"
	"testing"

	"arc-ir/pkg/ir"
)

const loopSrc = `
; fill an arena
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

func TestParseLoop(t *testing.T) {
	prog, err := Parse(loopSrc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// 3 labels + 11 instructions
	if len(prog.Instrs) != 14 {
		t.Fatalf("got %d instructions, want 14", len(prog.Instrs))
	}

	create := prog.Instrs[1]
	if create.Op != ir.OpArenaCreate || create.Dst != 1 || create.Size != 128 || create.Tier != ir.CXLPooled {
		t.Errorf("ARENA_CREATE parsed as %+v", create)
	}

	alloc := prog.Instrs[2]
	if alloc.Op != ir.OpArenaAlloc || alloc.Dst != 2 || alloc.Src[0] != 1 || alloc.Size != 128 {
		t.Errorf("ARENA_ALLOC parsed as %+v", alloc)
	}

	store := prog.Instrs[6]
	if store.Op != ir.OpStore {
		t.Fatalf("expected STORE, got %v", store.Op)
	}
	if store.Mem.Base != 2 || store.Mem.Index != 3 || store.Mem.Scale != 8 || store.Mem.Offset != 0 {
		t.Errorf("STORE mem = %+v", store.Mem)
	}
	if store.Src[0] != 3 {
		t.Errorf("STORE src = %v", store.Src[0])
	}

	jge := prog.Instrs[5]
	if jge.Op != ir.OpJge || jge.Src[0] != 3 || jge.Imm != 10 || jge.Label != "done" {
		t.Errorf("JGE parsed as %+v", jge)
	}

	load := prog.Instrs[10]
	if load.Op != ir.OpLoad || load.Dst != 4 {
		t.Fatalf("expected LOAD into r4, got %+v", load)
	}
	if load.Mem.Base != 2 || load.Mem.Index != 3 || load.Mem.Scale != 8 || load.Mem.Offset != -8 {
		t.Errorf("LOAD mem = %+v", load.Mem)
	}
}

func TestParseMovForms(t *testing.T) {
	prog, err := Parse("r1 = 5\nr2 = r1\nr3 = -7\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if prog.Instrs[0].Op != ir.OpMov || prog.Instrs[0].Imm != 5 || prog.Instrs[0].Src[0].Valid() {
		t.Errorf("imm mov parsed as %+v", prog.Instrs[0])
	}
	if prog.Instrs[1].Op != ir.OpMov || prog.Instrs[1].Src[0] != 1 {
		t.Errorf("reg mov parsed as %+v", prog.Instrs[1])
	}
	if prog.Instrs[2].Imm != -7 {
		t.Errorf("negative imm = %d", prog.Instrs[2].Imm)
	}
}

func TestParseFunc(t *testing.T) {
	src := "func main:\n" +
		"    r1 = 3\n" +
		"    r2 = 4\n" +
		"    CALL add\n" +
		"    RET r0\n" +
		"func add(r1: scalar, r2: scalar):\n" +
		"    r0 = ADD r1, r2\n" +
		"    RET r0\n"
	prog, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fn := prog.Instrs[0]
	if fn.Op != ir.OpFunc || fn.Label != "main" || len(fn.Params) != 0 {
		t.Errorf("main decl = %+v", fn)
	}
	call := prog.Instrs[3]
	if call.Op != ir.OpCall || call.Label != "add" {
		t.Errorf("CALL parsed as %+v", call)
	}
	add := prog.Instrs[5]
	if add.Op != ir.OpFunc || add.Label != "add" || len(add.Params) != 2 {
		t.Fatalf("add decl = %+v", add)
	}
	if add.Params[0].Reg != 1 || add.Params[0].Kind != ir.KindScalar {
		t.Errorf("param 0 = %+v", add.Params[0])
	}
	if add.Params[1].Reg != 2 || add.Params[1].Kind != ir.KindScalar {
		t.Errorf("param 1 = %+v", add.Params[1])
	}
}

func TestParsePtrParam(t *testing.T) {
	prog, err := Parse("func f(r1: ptr(64)):\n    RET\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := prog.Instrs[0].Params[0]
	if p.Kind != ir.KindPtr || p.Size != 64 || p.Reg != 1 {
		t.Errorf("ptr param parsed as %+v", p)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"bad register", "r99 = 1\n", "out of range"},
		{"unknown opcode", "r1 = FROB r2, 3\n", "unknown instruction"},
		{"bad scale", "STORE [r1 + r2*3], r3\n", "scale must be"},
		{"missing bracket", "r1 = LOAD r2\n", "expected"},
		{"trailing junk", "r1 = 1 2\n", "unexpected token"},
		{"ptr without size", "func f(r1: ptr):\n    RET\n", "requires a size"},
		{"unknown param type", "func f(r1: blob):\n    RET\n", "unknown parameter type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
