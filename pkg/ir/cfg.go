package ir

import "fmt"

// Block is a basic block: a label followed by a straight-line instruction
// sequence ending in a terminator (or falling through to the next block).
type Block struct {
	Label  string
	Index  int
	Instrs []Instruction
}

// Function is a single function: its declared parameters and its control-flow
// graph. Labels are local to the function.
type Function struct {
	Name      string
	Params    []Param
	RetKind   ValKind
	RetScalar ScalarKind
	RetSize   int64
	RetStruct string
	RetElem   ScalarKind
	RetCount  int64
	RetReg    Register // region parameter for a pointer return (NoReg = none)
	Blocks    []*Block
	Index     map[string]int // local label -> block index
	Succs     [][]int
	Preds     [][]int
	IsEntry   bool
	Inline    bool   // the backend inlines every call to this function
	Extern    bool   // a declared-only foreign (C) function: no body
	Symbol    string // Extern: the linked symbol
	RetVoid   bool   // Extern: the foreign function returns no value

	instrs []Instruction
}

// Module is a program split into functions.
type Module struct {
	Funcs   []*Function
	ByName  map[string]*Function
	Entry   *Function
	Structs map[string]*Struct
	Enums   map[string]*Enum
}

// BuildModule splits a program into functions, then builds each function's
// CFG.
//
// A program without any `func` declaration is treated as a single entry
// function named "main". When `func` declarations are present, the entry is
// the function named "main".
func BuildModule(p *Program) (*Module, error) {
	raw, err := splitFunctions(p)
	if err != nil {
		return nil, err
	}

	m := &Module{ByName: map[string]*Function{}, Structs: p.Structs, Enums: p.Enums}
	for _, r := range raw {
		if _, dup := m.ByName[r.name]; dup {
			return nil, fmt.Errorf("duplicate function %q", r.name)
		}
		if r.extern && len(r.instrs) > 0 {
			return nil, lineErr(r.instrs[0].Line, fmt.Errorf("extern function %q must not have a body", r.name))
		}
		f := &Function{Name: r.name, Params: r.params, RetKind: r.retKind, RetScalar: r.retScalar, RetSize: r.retSize, RetStruct: r.retStruct, RetElem: r.retElem, RetCount: r.retCount, RetReg: r.retReg, Index: map[string]int{}, Inline: r.inline, Extern: r.extern, Symbol: r.symbol, RetVoid: r.retVoid, instrs: r.instrs}
		m.ByName[r.name] = f
		m.Funcs = append(m.Funcs, f)
	}

	if len(m.Funcs) == 0 {
		return nil, fmt.Errorf("program is empty")
	}
	if f, ok := m.ByName["main"]; ok {
		m.Entry = f
	} else if len(m.Funcs) == 1 {
		m.Entry = m.Funcs[0]
	} else {
		return nil, fmt.Errorf("no entry function: expected a function named %q", "main")
	}
	if m.Entry.Extern {
		return nil, fmt.Errorf("no entry function: %q is an extern declaration", m.Entry.Name)
	}
	m.Entry.IsEntry = true

	for _, f := range m.Funcs {
		if f.Extern {
			continue
		}
		if err := buildBlocks(f); err != nil {
			return nil, err
		}
	}

	// Resolve CALL targets now that every function is known.
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for _, inst := range b.Instrs {
				if inst.Op == OpCall {
					if _, ok := m.ByName[inst.Label]; !ok {
						return nil, lineErr(inst.Line, fmt.Errorf("call to undefined function %q", inst.Label))
					}
				}
			}
		}
	}
	return m, nil
}

type rawFunc struct {
	name      string
	params    []Param
	retKind   ValKind
	retScalar ScalarKind
	retSize   int64
	retStruct string
	retElem   ScalarKind
	retCount  int64
	retReg    Register
	inline    bool
	extern    bool
	symbol    string
	retVoid   bool
	instrs    []Instruction
}

func splitFunctions(p *Program) ([]rawFunc, error) {
	var funcs []rawFunc
	var leading []Instruction
	cur := -1

	for _, inst := range p.Instrs {
		if inst.Op == OpFunc {
			funcs = append(funcs, rawFunc{
				name:      inst.Label,
				params:    inst.Params,
				retKind:   inst.RetKind,
				retScalar: inst.RetScalar,
				retSize:   inst.RetSize,
				retStruct: inst.RetStruct,
				retElem:   inst.RetElem,
				retCount:  inst.RetCount,
				retReg:    inst.RetReg,
				inline:    inst.Inline,
				extern:    inst.Extern,
				symbol:    inst.Symbol,
				retVoid:   inst.RetVoid,
			})
			cur = len(funcs) - 1
			continue
		}
		if cur < 0 {
			leading = append(leading, inst)
			continue
		}
		funcs[cur].instrs = append(funcs[cur].instrs, inst)
	}

	if len(funcs) == 0 {
		// No `func` declarations: the whole program is the entry function.
		return []rawFunc{{name: "main", instrs: leading}}, nil
	}
	if len(leading) > 0 {
		return nil, lineErr(leading[0].Line, fmt.Errorf("instruction outside any function (missing `func`)"))
	}
	return funcs, nil
}

// buildBlocks splits a function into basic blocks and computes its CFG.
func buildBlocks(f *Function) error {
	var cur *Block
	start := func(label string) error {
		if _, dup := f.Index[label]; dup {
			return fmt.Errorf("duplicate label %q in function %s", label, f.Name)
		}
		b := &Block{Label: label, Index: len(f.Blocks)}
		f.Index[label] = b.Index
		f.Blocks = append(f.Blocks, b)
		cur = b
		return nil
	}

	for _, inst := range f.instrs {
		if inst.Op == OpLabel {
			if err := start(inst.Label); err != nil {
				return lineErr(inst.Line, err)
			}
			continue
		}
		if cur == nil {
			if err := start("entry"); err != nil {
				return err
			}
		}
		if n := len(cur.Instrs); n > 0 && cur.Instrs[n-1].Op.IsTerminator() {
			return lineErr(inst.Line,
				fmt.Errorf("unreachable instruction after %s", cur.Instrs[n-1].Op))
		}
		cur.Instrs = append(cur.Instrs, inst)
	}

	if len(f.Blocks) == 0 {
		return lineErr(0, fmt.Errorf("function %s has no instructions", f.Name))
	}

	// A function may not fall off its end: the last block must transfer control
	// explicitly, so every exit reaches a RET (or loops forever via JMP). This
	// matters for safety: a fall-through would otherwise return garbage (the
	// codegen synthesises a 0), which for a pointer return is a null dereference.
	last := f.Blocks[len(f.Blocks)-1]
	if n := len(last.Instrs); n == 0 || !last.Instrs[n-1].Op.IsTerminator() {
		return lineErr(lastInstrLine(f), fmt.Errorf("function %s may fall off its end: add an explicit RET", f.Name))
	}

	f.Succs = make([][]int, len(f.Blocks))
	f.Preds = make([][]int, len(f.Blocks))
	for bi, b := range f.Blocks {
		var succ []int
		for _, inst := range b.Instrs {
			if inst.Op.IsCondJump() {
				ti, err := f.labelIndex(inst)
				if err != nil {
					return err
				}
				succ = append(succ, ti)
			}
		}

		next := bi + 1
		terminated := false
		if n := len(b.Instrs); n > 0 {
			last := b.Instrs[n-1]
			switch {
			case last.Op == OpRet:
				terminated = true
			case last.Op == OpJmp:
				ti, err := f.labelIndex(last)
				if err != nil {
					return err
				}
				succ = append(succ, ti)
				terminated = true
			}
		}
		if !terminated && next < len(f.Blocks) {
			succ = append(succ, next)
		}

		f.Succs[bi] = dedupe(succ)
		for _, s := range f.Succs[bi] {
			f.Preds[s] = append(f.Preds[s], bi)
		}
	}
	return nil
}

// lastInstrLine returns the source line of the last instruction in the
// function, searching backwards across blocks so that an error about the end
// of the function points somewhere useful (the final block may be empty).
func lastInstrLine(f *Function) int {
	for bi := len(f.Blocks) - 1; bi >= 0; bi-- {
		if n := len(f.Blocks[bi].Instrs); n > 0 {
			return f.Blocks[bi].Instrs[n-1].Line
		}
	}
	return 0
}

func (f *Function) labelIndex(inst Instruction) (int, error) {
	ti, ok := f.Index[inst.Label]
	if !ok {
		return 0, lineErr(inst.Line, fmt.Errorf("jump to undefined label %q in function %s", inst.Label, f.Name))
	}
	return ti, nil
}

func dedupe(xs []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func lineErr(line int, err error) error {
	if line > 0 {
		return fmt.Errorf("line %d: %w", line, err)
	}
	return err
}

// UseDef reports the registers an instruction reads (use) and writes (def).
//
// A call is modelled through its callee: the argument registers are read, and
// every scratch and vector register is written, because a call clobbers the
// volatile files (see the verifier). Preserved registers are not written — the
// callee saves the ones it writes. The module is needed to resolve CALL targets.
func (m *Module) UseDef(inst *Instruction) (use, def map[Register]bool) {
	use, def = map[Register]bool{}, map[Register]bool{}
	addUse := func(r Register) {
		if r.Valid() {
			use[r] = true
		}
	}
	addDef := func(r Register) {
		if r.Valid() {
			def[r] = true
		}
	}

	addUse(inst.Src[0])
	addUse(inst.Src[1])

	switch inst.Op {
	case OpLabel, OpFunc:
		// pseudo-instructions: no operands
	case OpLoad:
		addUse(inst.Mem.Base)
		addUse(inst.Mem.Index)
		addDef(inst.Dst)
	case OpStore:
		addUse(inst.Mem.Base)
		addUse(inst.Mem.Index)
	case OpCall:
		callee := m.ByName[inst.Label]
		if callee != nil {
			for _, p := range callee.Params {
				addUse(p.Reg)
			}
		}
		for r := 0; r < NumScratch; r++ {
			addDef(Register(r))
		}
		for r := NumGP; r < NumRegs; r++ {
			addDef(Register(r))
		}
		switch {
		case callee == nil:
			addDef(0)
		case callee.RetVoid:
			// a void foreign call defines no result (scratch is clobbered above)
		case callee.Extern && callee.RetKind == KindScalar && callee.RetScalar.Float():
			addDef(Register(NumGP)) // float result in v0
		default:
			addDef(0) // result in r0
		}
	default:
		if inst.Op.HasDest() {
			addDef(inst.Dst)
		}
	}
	return use, def
}

// LiveOut computes, for each instruction of f, the set of registers live
// immediately after it. A register is live at a point when some path from that
// point reads it before writing it. The backward dataflow runs to a fixpoint so
// that values live across a back edge (a loop) are seen.
//
// The backend uses this to spill only the registers a call actually clobbers,
// instead of every register the function happens to use.
func (m *Module) LiveOut(f *Function) map[*Instruction]map[Register]bool {
	nb := len(f.Blocks)

	// Per-block use/def: a register is in the block's use set only if it is read
	// before any write in the block.
	useB := make([]map[Register]bool, nb)
	defB := make([]map[Register]bool, nb)
	for bi, b := range f.Blocks {
		useB[bi], defB[bi] = map[Register]bool{}, map[Register]bool{}
		for i := range b.Instrs {
			u, d := m.UseDef(&b.Instrs[i])
			for r := range u {
				if !defB[bi][r] {
					useB[bi][r] = true
				}
			}
			for r := range d {
				defB[bi][r] = true
			}
		}
	}

	liveIn := make([]map[Register]bool, nb)
	liveOut := make([]map[Register]bool, nb)
	for bi := range liveIn {
		liveIn[bi] = map[Register]bool{}
		liveOut[bi] = map[Register]bool{}
	}
	for changed := true; changed; {
		changed = false
		for bi := nb - 1; bi >= 0; bi-- {
			out := map[Register]bool{}
			for _, s := range f.Succs[bi] {
				for r := range liveIn[s] {
					out[r] = true
				}
			}
			in := map[Register]bool{}
			for r := range useB[bi] {
				in[r] = true
			}
			for r := range out {
				if !defB[bi][r] {
					in[r] = true
				}
			}
			if !sameRegs(out, liveOut[bi]) {
				liveOut[bi] = out
				changed = true
			}
			if !sameRegs(in, liveIn[bi]) {
				liveIn[bi] = in
				changed = true
			}
		}
	}

	// Walk each block backward to turn the block-level sets into per-instruction
	// live-out sets.
	res := make(map[*Instruction]map[Register]bool)
	for bi, b := range f.Blocks {
		out := cloneRegs(liveOut[bi])
		for i := len(b.Instrs) - 1; i >= 0; i-- {
			inst := &b.Instrs[i]
			res[inst] = out
			u, d := m.UseDef(inst)
			next := make(map[Register]bool, len(out)+len(u))
			for r := range out {
				if !d[r] {
					next[r] = true
				}
			}
			for r := range u {
				next[r] = true
			}
			out = next
		}
	}
	return res
}

func sameRegs(a, b map[Register]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for r := range a {
		if !b[r] {
			return false
		}
	}
	return true
}

func cloneRegs(a map[Register]bool) map[Register]bool {
	c := make(map[Register]bool, len(a))
	for r := range a {
		c[r] = true
	}
	return c
}
