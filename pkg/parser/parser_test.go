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
	prog, err := Parse("func f(r1: ptr([i64; 8])):\n    RET\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := prog.Instrs[0].Params[0]
	if p.Kind != ir.KindPtr || p.Size != 64 || p.Reg != 1 {
		t.Errorf("ptr param parsed as %+v", p)
	}
}

func TestParseReturnType(t *testing.T) {
	src := "func f() -> arena:\n    RET\n" +
		"func g() -> ptr([i64; 8]):\n    RET\n" +
		"func h() -> scalar:\n    RET\n"
	prog, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f := prog.Instrs[0]; f.RetKind != ir.KindArena {
		t.Errorf("f return = %v", f.RetKind)
	}
	if g := prog.Instrs[2]; g.RetKind != ir.KindPtr || g.RetSize != 64 {
		t.Errorf("g return = %v/%d", g.RetKind, g.RetSize)
	}
	if h := prog.Instrs[4]; h.RetKind != ir.KindScalar {
		t.Errorf("h return = %v", h.RetKind)
	}
}

func TestParseOwnParam(t *testing.T) {
	prog, err := Parse("func f(r1: own arena, r2: arena):\n    RET\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p0 := prog.Instrs[0].Params[0]
	if p0.Kind != ir.KindArena || !p0.Own {
		t.Errorf("own param parsed as %+v", p0)
	}
	p1 := prog.Instrs[0].Params[1]
	if p1.Kind != ir.KindArena || p1.Own {
		t.Errorf("borrow param parsed as %+v", p1)
	}
}

func TestParseGrowableArena(t *testing.T) {
	prog, err := Parse("r1 = ARENA_CREATE DRAM_LOCAL, growable(4096)\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	in := prog.Instrs[0]
	if in.Op != ir.OpArenaCreate || !in.Growable || in.Size != 4096 || in.Tier != ir.DRAMLocal {
		t.Errorf("growable arena parsed as %+v", in)
	}

	fixed, err := Parse("r1 = ARENA_CREATE CXL_POOLED, 128\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if fixed.Instrs[0].Growable || fixed.Instrs[0].Size != 128 {
		t.Errorf("fixed arena parsed as %+v", fixed.Instrs[0])
	}
}

func TestParseFloat(t *testing.T) {
	src := "v1 = 2.5\nv2 = -1.5\nv3 = FADD v1, v2\nr1 = FTOI v3\nv4 = ITOF r1\nPRINTF v4\n"
	prog, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if in := prog.Instrs[0]; in.Op != ir.OpFConst || in.FImm != 2.5 || !in.Dst.IsVec() {
		t.Errorf("float const parsed as %+v", in)
	}
	if in := prog.Instrs[1]; in.FImm != -1.5 {
		t.Errorf("negative float = %v", in.FImm)
	}
	if in := prog.Instrs[2]; in.Op != ir.OpFAdd || in.Src[0] != ir.Register(ir.NumGP+1) {
		t.Errorf("FADD parsed as %+v", in)
	}
	if in := prog.Instrs[3]; in.Op != ir.OpFToI {
		t.Errorf("FTOI parsed as %+v", in)
	}
	if in := prog.Instrs[4]; in.Op != ir.OpIToF {
		t.Errorf("ITOF parsed as %+v", in)
	}
	if in := prog.Instrs[5]; in.Op != ir.OpPrintF {
		t.Errorf("PRINTF parsed as %+v", in)
	}
}

func TestParseF32(t *testing.T) {
	prog, err := Parse("v1 = f32(2.5)\nv2 = FADD32 v1, v1\nr1 = F32TOI v2\nv3 = ITOF32 r1\nPRINTF32 v3\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if in := prog.Instrs[0]; in.Op != ir.OpFConst || !in.F32 || in.FImm != 2.5 {
		t.Errorf("f32 const parsed as %+v", in)
	}
	if in := prog.Instrs[1]; in.Op != ir.OpF32Add {
		t.Errorf("FADD32 parsed as %+v", in)
	}
	if in := prog.Instrs[2]; in.Op != ir.OpF32ToI {
		t.Errorf("F32TOI parsed as %+v", in)
	}
	if in := prog.Instrs[3]; in.Op != ir.OpIToF32 {
		t.Errorf("ITOF32 parsed as %+v", in)
	}
	if in := prog.Instrs[4]; in.Op != ir.OpPrintF32 {
		t.Errorf("PRINTF32 parsed as %+v", in)
	}
}

func TestParseSIMD(t *testing.T) {
	prog, err := Parse("v1 = f32x4(1.0, 2.0, 3.0, 4.0)\nv2 = i32x4(7)\nv3 = VADD.f32x4 v1, v1\nPRINTV.i32x4 v2\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if in := prog.Instrs[0]; in.Op != ir.OpVConst || in.Vec != ir.VecF32x4 || len(in.Lanes) != 4 {
		t.Errorf("f32x4 const parsed as %+v", in)
	}
	if in := prog.Instrs[1]; in.Op != ir.OpVConst || in.Vec != ir.VecI32x4 || len(in.Lanes) != 4 || in.Lanes[3] != 7 {
		t.Errorf("i32x4 broadcast parsed as %+v", in)
	}
	if in := prog.Instrs[2]; in.Op != ir.OpVAdd || in.Vec != ir.VecF32x4 {
		t.Errorf("VADD.f32x4 parsed as %+v", in)
	}
	if in := prog.Instrs[3]; in.Op != ir.OpPrintV || in.Vec != ir.VecI32x4 {
		t.Errorf("PRINTV.i32x4 parsed as %+v", in)
	}
}

func TestParseSIMD64(t *testing.T) {
	prog, err := Parse("v1 = f64x2(1.5, 2.5)\nv2 = i64x2(9)\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if in := prog.Instrs[0]; in.Op != ir.OpVConst || in.Vec != ir.VecF64x2 || len(in.Lanes) != 2 {
		t.Errorf("f64x2 const parsed as %+v", in)
	}
	if in := prog.Instrs[1]; in.Op != ir.OpVConst || in.Vec != ir.VecI64x2 || len(in.Lanes) != 2 || in.Lanes[1] != 9 {
		t.Errorf("i64x2 broadcast parsed as %+v", in)
	}
}

func TestParseStruct(t *testing.T) {
	src := "struct Node { next: ptr([i64; 1]), val: scalar }\n" +
		"func main:\n" +
		"    r2 = ARENA_ALLOC r1, Node\n" +
		"    r3 = r2.next\n" +
		"    r2.val = r3\n" +
		"    RET\n"
	prog, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sf := prog.Structs["Node"]
	if sf == nil || len(sf.Fields) != 2 {
		t.Fatalf("struct parsed as %+v", sf)
	}
	if sf.Fields[0].Name != "next" || sf.Fields[0].Type != ir.KindPtr || sf.Fields[0].Size != 8 {
		t.Errorf("field 0 = %+v", sf.Fields[0])
	}
	if sf.Size() != 16 {
		t.Errorf("sizeof = %d, want 16", sf.Size())
	}
	if in := prog.Instrs[1]; in.Op != ir.OpArenaAlloc || in.Struct != "Node" || in.Size != 16 {
		t.Errorf("alloc parsed as %+v", in)
	}
	if in := prog.Instrs[2]; in.Op != ir.OpField || in.Field != "next" || in.Src[0] != 2 {
		t.Errorf("field load parsed as %+v", in)
	}
	if in := prog.Instrs[3]; in.Op != ir.OpFieldStore || in.Field != "val" || in.Src[0] != 2 || in.Src[1] != 3 {
		t.Errorf("field store parsed as %+v", in)
	}
}

func TestParsePtrStruct(t *testing.T) {
	prog, err := Parse("struct Node { next: ptr([i64; 1]), val: scalar }\nfunc f(r1: ptr(Node)):\n    RET\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := prog.Instrs[0].Params[0]
	if p.Kind != ir.KindPtr || p.Struct != "Node" || p.Size != 16 {
		t.Errorf("ptr(Node) parsed as %+v", p)
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
		{"raw ptr rejected", "func f(r1: ptr(64)):\n    RET\n", "ptr needs a struct or array"},
		{"raw ptr return rejected", "func f() -> ptr(64):\n    RET\n", "ptr needs a struct or array"},
		{"unknown param type", "func f(r1: blob):\n    RET\n", "unknown parameter type"},
		{"own on non-arena", "func f(r1: own scalar):\n    RET\n", "only valid for arena"},
		{"missing destination", "ARENA_CREATE DRAM_LOCAL, 1\n", "requires a destination"},
		{"unexpected destination", "r1 = PRINT r2\n", "does not produce a result"},
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
