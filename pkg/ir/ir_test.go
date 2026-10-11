package ir

import "testing"

func TestOpcodeRoundtrip(t *testing.T) {
	for _, name := range []string{"MOV", "ARENA_CREATE", "ARENA_ALLOC", "STORE", "LOAD", "ADD", "JGE", "JMP", "RET"} {
		op, ok := OpFromName(name)
		if !ok {
			t.Fatalf("OpFromName(%q) not found", name)
		}
		if op.String() != name {
			t.Errorf("roundtrip %q -> %q", name, op.String())
		}
	}
	if _, ok := OpFromName("NOPE"); ok {
		t.Error("OpFromName(NOPE) unexpectedly found")
	}
}

func TestOpcodeClassification(t *testing.T) {
	if !OpJmp.IsTerminator() || !OpRet.IsTerminator() {
		t.Error("JMP and RET must be terminators")
	}
	if OpJge.IsTerminator() {
		t.Error("conditional jump must not be a terminator")
	}
	if !OpJge.IsCondJump() || !OpJeq.IsCondJump() {
		t.Error("expected conditional jumps")
	}
	if !OpArenaAlloc.HasDest() || OpStore.HasDest() || OpRet.HasDest() {
		t.Error("HasDest classification is wrong")
	}
}

func TestRegisterString(t *testing.T) {
	if got := Register(3).String(); got != "r3" {
		t.Errorf("Register(3).String() = %q", got)
	}
	if NoReg.String() != "<none>" {
		t.Errorf("NoReg.String() = %q", NoReg.String())
	}
}

func TestMemString(t *testing.T) {
	m := Mem{Base: 2, Index: 3, Scale: 8, Offset: 16}
	if got := m.String(); got != "[r2 + r3*8 + 16]" {
		t.Errorf("Mem.String() = %q", got)
	}
	m = Mem{Base: 2, Index: NoReg, Scale: 1, Offset: -8}
	if got := m.String(); got != "[r2 - 8]" {
		t.Errorf("Mem.String() = %q", got)
	}
}

func TestRHS(t *testing.T) {
	reg := Instruction{Src: [2]Register{1, 2}}
	if r, _, isReg := reg.RHS(); !isReg || r != 2 {
		t.Errorf("RHS register case failed: %v %v", r, isReg)
	}
	imm := Instruction{Src: [2]Register{1, NoReg}, Imm: 42}
	if r, v, isReg := imm.RHS(); isReg || r != NoReg || v != 42 {
		t.Errorf("RHS immediate case failed: %v %v %v", r, v, isReg)
	}
}

func buildProgram(instrs ...Instruction) *Program {
	return &Program{Instrs: instrs}
}

func TestFuncString(t *testing.T) {
	// The return type is always rendered, even for the i64 default.
	scalar := Instruction{Op: OpFunc, Label: "f", RetKind: KindScalar, RetScalar: I64}
	if got, want := scalar.String(), "func f() -> i64:"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	ptr := Instruction{Op: OpFunc, Label: "g", RetKind: KindPtr, RetElem: I64, RetCount: 8, RetSize: 64}
	if got, want := ptr.String(), "func g() -> ptr([i64; 8]):"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestBuildFunctionCFG(t *testing.T) {
	// entry falls through into a conditional jump block that targets "done".
	prog := buildProgram(
		Instruction{Op: OpLabel, Label: "entry"},
		Instruction{Op: OpMov, Dst: 1, Imm: 0},
		Instruction{Op: OpLabel, Label: "loop"},
		Instruction{Op: OpJge, Src: [2]Register{1, NoReg}, Imm: 10, Label: "done"},
		Instruction{Op: OpJmp, Label: "loop"},
		Instruction{Op: OpLabel, Label: "done"},
		Instruction{Op: OpRet, Src: [2]Register{1, NoReg}},
	)
	f, err := BuildModule(prog)
	if err != nil {
		t.Fatalf("BuildModule: %v", err)
	}
	entry := f.Entry
	if len(entry.Blocks) != 3 {
		t.Fatalf("got %d blocks, want 3", len(entry.Blocks))
	}
	// entry -> loop
	if got := entry.Succs[entry.Index["entry"]]; len(got) != 1 || got[0] != entry.Index["loop"] {
		t.Errorf("entry succs = %v", got)
	}
	// loop -> done (conditional) and loop (jmp)
	loop := entry.Index["loop"]
	if len(entry.Succs[loop]) != 2 {
		t.Fatalf("loop succs = %v, want 2", entry.Succs[loop])
	}
	// done has no successors (RET)
	if got := entry.Succs[entry.Index["done"]]; len(got) != 0 {
		t.Errorf("done succs = %v, want none", got)
	}
}

func TestBuildFunctionErrors(t *testing.T) {
	cases := []struct {
		name string
		prog *Program
	}{
		{"duplicate label", buildProgram(
			Instruction{Op: OpLabel, Label: "a"},
			Instruction{Op: OpLabel, Label: "a"},
		)},
		{"undefined target", buildProgram(
			Instruction{Op: OpLabel, Label: "a"},
			Instruction{Op: OpJmp, Label: "missing"},
		)},
		{"code after terminator", buildProgram(
			Instruction{Op: OpLabel, Label: "a"},
			Instruction{Op: OpRet},
			Instruction{Op: OpMov, Dst: 1, Imm: 1},
		)},
		{"falls off end", buildProgram(
			Instruction{Op: OpLabel, Label: "a"},
			Instruction{Op: OpMov, Dst: 1, Imm: 1},
		)},
		{"falls off end at conditional", buildProgram(
			Instruction{Op: OpLabel, Label: "a"},
			Instruction{Op: OpMov, Dst: 1, Imm: 1},
			Instruction{Op: OpJeq, Src: [2]Register{1, NoReg}, Imm: 0, Label: "a"},
		)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildModule(tc.prog); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
