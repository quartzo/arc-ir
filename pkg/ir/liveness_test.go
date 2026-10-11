package ir

import "testing"

// liveOutAt returns the LiveOut map and locates an instruction by scanning the
// entry function's blocks; tests use it to avoid depending on instruction
// pointers from the pre-split program.
func liveOutEntry(t *testing.T, prog *Program) (*Module, *Function, map[*Instruction]map[Register]bool) {
	t.Helper()
	m, err := BuildModule(prog)
	if err != nil {
		t.Fatalf("BuildModule: %v", err)
	}
	return m, m.Entry, m.LiveOut(m.Entry)
}

func TestLiveOutStraightLine(t *testing.T) {
	prog := buildProgram(
		Instruction{Op: OpLabel, Label: "entry"},
		Instruction{Op: OpMov, Dst: 1, Imm: 0},
		Instruction{Op: OpAdd, Dst: 2, Src: [2]Register{1, NoReg}, Imm: 1},
		Instruction{Op: OpPrint, Src: [2]Register{2, NoReg}},
		Instruction{Op: OpRet, Src: [2]Register{2, NoReg}},
	)
	_, f, live := liveOutEntry(t, prog)
	b := f.Blocks[0]
	mov, add, print := &b.Instrs[0], &b.Instrs[1], &b.Instrs[2]

	if !live[mov][1] {
		t.Error("r1 must be live after its definition (read by the ADD)")
	}
	if live[add][1] {
		t.Error("r1 must be dead after the ADD (no later use)")
	}
	if !live[add][2] || !live[print][2] {
		t.Error("r2 must be live from the ADD through the PRINT to the RET")
	}
}

func TestLiveOutCallKillsScratch(t *testing.T) {
	prog := buildProgram(
		Instruction{Op: OpFunc, Label: "main", RetKind: KindScalar, RetScalar: I64},
		Instruction{Op: OpMov, Dst: 2, Imm: 6},
		Instruction{Op: OpCall, Label: "leaf"},
		Instruction{Op: OpRet, Src: [2]Register{2, NoReg}},
		Instruction{Op: OpFunc, Label: "leaf", RetKind: KindScalar, RetScalar: I64},
		Instruction{Op: OpRet},
	)
	_, f, live := liveOutEntry(t, prog)
	b := f.Blocks[0]
	mov, call := &b.Instrs[0], &b.Instrs[1]

	// The call destroys every scratch register, so r2 is not live across it: the
	// value defined before the call cannot be the one read by the RET.
	if live[mov][2] {
		t.Error("a scratch register must not be live across a call")
	}
	// It is still live after the call (the RET re-reads the register), which is
	// why a spill decision must also exclude what the call defines.
	if !live[call][2] {
		t.Error("r2 must be live after the call for the RET")
	}
}

func TestLiveOutPreservedSurvivesCall(t *testing.T) {
	prog := buildProgram(
		Instruction{Op: OpFunc, Label: "main", RetKind: KindScalar, RetScalar: I64},
		Instruction{Op: OpMov, Dst: Register(NumScratch), Imm: 5},
		Instruction{Op: OpCall, Label: "leaf"},
		Instruction{Op: OpRet, Src: [2]Register{Register(NumScratch), NoReg}},
		Instruction{Op: OpFunc, Label: "leaf", RetKind: KindScalar, RetScalar: I64},
		Instruction{Op: OpRet},
	)
	_, f, live := liveOutEntry(t, prog)
	mov := &f.Blocks[0].Instrs[0]
	if !live[mov][Register(NumScratch)] {
		t.Error("a preserved register must stay live across a call")
	}
}

func TestLiveOutLoopBackEdge(t *testing.T) {
	prog := buildProgram(
		Instruction{Op: OpLabel, Label: "entry"},
		Instruction{Op: OpMov, Dst: 1, Imm: 0},
		Instruction{Op: OpLabel, Label: "loop"},
		Instruction{Op: OpAdd, Dst: 1, Src: [2]Register{1, NoReg}, Imm: 1},
		Instruction{Op: OpJlt, Src: [2]Register{1, NoReg}, Imm: 5, Label: "loop"},
		Instruction{Op: OpRet, Src: [2]Register{1, NoReg}},
	)
	_, f, live := liveOutEntry(t, prog)
	add := &f.Blocks[1].Instrs[0]
	// r1 is re-read by the next iteration (through the back edge), so it is live
	// across the ADD that defines it.
	if !live[add][1] {
		t.Error("a value live across a back edge must be reported live")
	}
}
