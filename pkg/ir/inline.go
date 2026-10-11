package ir

import "fmt"

// Inline expands every call to a function marked inline, for the whole module.
//
// Inlining is safe without register renaming only when the callee uses no
// preserved registers (s*): scratch and vector registers are volatile across a
// call, so the caller holds no live value in them across the call and the callee
// may clobber them freely. A callee that uses a preserved register, or that is
// recursive, is left as a real call.
//
// It runs after verification, so the callee already carries the automatic
// release (ARENA_DESTROY) at its RET; inlining keeps those instructions, so the
// region is still released at the point that was the callee's return.
func Inline(m *Module) error {
	for _, f := range m.Funcs {
		if err := inlineFunc(m, f); err != nil {
			return err
		}
	}
	dropUnusedInlineFuncs(m)
	return nil
}

// dropUnusedInlineFuncs removes inline functions that no longer have any call
// site (they were inlined everywhere). An inline function that was left as a
// real call — because it uses a preserved register, or is recursive — is kept.
func dropUnusedInlineFuncs(m *Module) {
	called := map[string]bool{}
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for _, inst := range b.Instrs {
				if inst.Op == OpCall {
					called[inst.Label] = true
				}
			}
		}
	}
	kept := m.Funcs[:0]
	for _, f := range m.Funcs {
		if f.Inline && !f.IsEntry && !called[f.Name] {
			delete(m.ByName, f.Name)
			continue
		}
		kept = append(kept, f)
	}
	m.Funcs = kept
}

func inlineFunc(m *Module, f *Function) error {
	seq := 0
	out, changed := inlineInstrs(m, linearize(f), map[string]bool{f.Name: true}, &seq)
	if !changed {
		return nil
	}
	f.instrs = out
	f.Blocks = nil
	f.Index = map[string]int{}
	f.Succs = nil
	f.Preds = nil
	if err := buildBlocks(f); err != nil {
		return fmt.Errorf("inlining into %s: %w", f.Name, err)
	}
	return nil
}

// inlineInstrs walks a linear instruction list and expands calls to inline
// functions. stack is the set of functions currently being inlined, so a
// recursive function is left as a call.
func inlineInstrs(m *Module, instrs []Instruction, stack map[string]bool, seq *int) ([]Instruction, bool) {
	var out, bodies []Instruction
	changed := false
	for _, inst := range instrs {
		if inst.Op == OpCall {
			if g := m.ByName[inst.Label]; g != nil && g.Inline && !stack[g.Name] && !usesPreserved(g) {
				*seq++
				prefix := fmt.Sprintf("__inline%d$", *seq)
				cont := fmt.Sprintf("__inline%d_ret", *seq)
				out = append(out,
					Instruction{Op: OpJmp, Label: prefix + g.Blocks[0].Label, Line: inst.Line},
					Instruction{Op: OpLabel, Label: cont, Line: inst.Line})
				body := renameLabels(linearize(g), prefix)
				body = replaceRets(body, cont)
				stack[g.Name] = true
				body, _ = inlineInstrs(m, body, stack, seq)
				delete(stack, g.Name)
				bodies = append(bodies, body...)
				changed = true
				continue
			}
		}
		out = append(out, inst)
	}
	return append(out, bodies...), changed
}

// linearize renders a function's blocks as a flat instruction list, with a label
// before each block. Feeding it back to buildBlocks reconstructs the same CFG.
func linearize(f *Function) []Instruction {
	var out []Instruction
	for _, b := range f.Blocks {
		out = append(out, Instruction{Op: OpLabel, Label: b.Label})
		out = append(out, b.Instrs...)
	}
	return out
}

// renameLabels prefixes every label definition and jump target.
func renameLabels(instrs []Instruction, prefix string) []Instruction {
	out := make([]Instruction, len(instrs))
	copy(out, instrs)
	for i := range out {
		switch {
		case out[i].Op == OpLabel, out[i].Op == OpJmp, out[i].Op.IsCondJump():
			out[i].Label = prefix + out[i].Label
		}
	}
	return out
}

// replaceRets turns each RET into "r0 = value; JMP cont": the value lands in r0
// as the calling convention expects, and control returns to the continuation.
func replaceRets(instrs []Instruction, cont string) []Instruction {
	var out []Instruction
	for _, inst := range instrs {
		if inst.Op != OpRet {
			out = append(out, inst)
			continue
		}
		if inst.Src[0].Valid() && inst.Src[0] != 0 {
			mv := Instruction{Op: OpMov, Dst: 0, Line: inst.Line}
			mv.Src = [2]Register{inst.Src[0], NoReg}
			mv.Mem.Base, mv.Mem.Index = NoReg, NoReg
			out = append(out, mv)
		}
		out = append(out, Instruction{Op: OpJmp, Label: cont, Line: inst.Line})
	}
	return out
}

// usesPreserved reports whether a function reads or writes a preserved (s*)
// register, which would require save/restore to inline safely.
func usesPreserved(f *Function) bool {
	check := func(r Register) bool { return r.Valid() && r.IsPreserved() }
	for _, b := range f.Blocks {
		for _, inst := range b.Instrs {
			if check(inst.Dst) || check(inst.Src[0]) || check(inst.Src[1]) ||
				check(inst.Mem.Base) || check(inst.Mem.Index) {
				return true
			}
		}
	}
	return false
}
