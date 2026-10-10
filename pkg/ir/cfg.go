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
	Blocks    []*Block
	Index     map[string]int // local label -> block index
	Succs     [][]int
	Preds     [][]int
	IsEntry   bool

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
		f := &Function{Name: r.name, Params: r.params, RetKind: r.retKind, RetScalar: r.retScalar, RetSize: r.retSize, RetStruct: r.retStruct, RetElem: r.retElem, RetCount: r.retCount, Index: map[string]int{}, instrs: r.instrs}
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
	m.Entry.IsEntry = true

	for _, f := range m.Funcs {
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
