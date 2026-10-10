// Package verifier implements an eBPF-style static verifier for ARC assembly.
//
// It is an abstract interpreter over the control-flow graph. For every program
// point it tracks, for each of the 16 registers, an abstract state:
//
//   - the value type (uninitialized, scalar, arena handle, data pointer),
//   - for scalars, a signed value range [Min, Max],
//   - for arena handles and data pointers, the owning arena and allocation
//     size (used for bounds checks),
//   - whether the owning arena has been destroyed.
//
// States are propagated along edges and merged at join points until a fixpoint
// is reached. Conditional jumps refine scalar ranges on the taken and
// fall-through edges (e.g. after "JGE r3, 10, exit" the fall-through knows
// r3 < 10). This lets the verifier prove that a dynamic access such as
// [r2 + r3*8] stays inside its allocation.
//
// Rejected programs include: out-of-bounds accesses, use-after-destroy,
// use of an uninitialized register, type misuse, and any access whose bounds
// cannot be proven.
package verifier

import (
	"fmt"
	"math"

	"arc-ir/pkg/ir"
)

// RegType is the abstract type of a register.
type RegType int

const (
	// TypeUnknown is the zero value (uninitialized, or conflicting at a merge).
	TypeUnknown RegType = iota
	// TypeScalar is a 64-bit integer with an associated range.
	TypeScalar
	// TypeArena is an arena handle.
	TypeArena
	// TypeDataPtr is a pointer into an arena allocation.
	TypeDataPtr
)

// String returns a human-readable type name.
func (t RegType) String() string {
	switch t {
	case TypeScalar:
		return "scalar"
	case TypeArena:
		return "arena"
	case TypeDataPtr:
		return "data-ptr"
	default:
		return "unknown"
	}
}

// RegState is the abstract state of one register.
type RegState struct {
	Defined   bool
	Type      RegType
	Min, Max  int64       // scalar range
	ArenaID   ir.Register // owning arena (data pointers)
	AllocSize int64       // reachable bytes (arena capacity or allocation size)
	Destroyed bool        // owning arena destroyed
}

// State is the abstract state of the whole register file.
type State [ir.NumRegs]RegState

// Result is the outcome of a successful verification.
type Result struct {
	Blocks int // total basic blocks verified
	Funcs  int // number of functions verified
}

// Error is a verifier diagnostic tied to a source line.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("verify error (line %d): %s", e.Line, e.Msg)
	}
	return "verify error: " + e.Msg
}

// Verify checks every function in a program.
func Verify(prog *ir.Program) (*Result, error) {
	m, err := ir.BuildModule(prog)
	if err != nil {
		return nil, err
	}

	res := &Result{Funcs: len(m.Funcs)}
	for _, f := range m.Funcs {
		v := &Verifier{mod: m, f: f, in: make([]*State, len(f.Blocks))}
		entry, err := v.entryState()
		if err != nil {
			return nil, err
		}
		v.in[0] = &entry
		work := []int{0}

		for len(work) > 0 {
			bi := work[0]
			work = work[1:]

			st := *v.in[bi]
			succ, err := v.processBlock(bi, &st)
			if err != nil {
				return nil, err
			}
			for target, out := range succ {
				if v.mergeInto(target, out) {
					work = append(work, target)
				}
			}
		}
		res.Blocks += len(f.Blocks)
	}
	return res, nil
}

// Verifier holds the fixpoint state of one function.
type Verifier struct {
	mod *ir.Module
	f   *ir.Function
	in  []*State
}

// entryState builds the initial register state of a function: empty for the
// entry function, or the declared parameter types for a callee.
func (v *Verifier) entryState() (State, error) {
	var st State
	if v.f.IsEntry {
		return st, nil
	}
	for _, p := range v.f.Params {
		switch p.Kind {
		case ir.KindScalar:
			st[p.Reg] = RegState{Defined: true, Type: TypeScalar, Min: math.MinInt64, Max: math.MaxInt64}
		case ir.KindArena:
			// Capacity is unknown at the callee; allocations from it are still
			// bounds-checked against their own size.
			st[p.Reg] = RegState{Defined: true, Type: TypeArena}
		case ir.KindPtr:
			st[p.Reg] = RegState{Defined: true, Type: TypeDataPtr, ArenaID: ir.NoReg, AllocSize: p.Size}
		default:
			return st, fmt.Errorf("function %s: parameter %s: unsupported type %s",
				v.f.Name, p.Reg, p.Kind)
		}
	}
	return st, nil
}

// processBlock runs a block from its entry state and returns the outgoing
// state for each successor edge. Conditional jumps refine the running state on
// both the taken edge (recorded immediately) and the fall-through.
func (v *Verifier) processBlock(bi int, st *State) (map[int]State, error) {
	b := v.f.Blocks[bi]
	out := map[int]State{}

	for _, inst := range b.Instrs {
		switch {
		case inst.Op == ir.OpRet:
			if inst.Src[0].Valid() {
				if err := v.checkReturn(st, inst.Src[0], inst); err != nil {
					return nil, err
				}
			} else if v.f.RetKind != ir.KindScalar {
				return nil, v.errf(inst, "function %s returns %s but RET has no value", v.f.Name, v.f.RetKind)
			}
			return out, nil

		case inst.Op == ir.OpJmp:
			out[v.f.Index[inst.Label]] = *st
			return out, nil

		case inst.Op.IsCondJump():
			if _, err := v.scalar(st, inst.Src[0], inst); err != nil {
				return nil, err
			}
			taken := refine(*st, inst, true)
			mergeEdge(out, v.f.Index[inst.Label], taken)
			*st = refine(*st, inst, false)

		default:
			if err := v.apply(inst, st); err != nil {
				return nil, err
			}
		}
	}

	// Fall through to the next block, if any.
	if bi+1 < len(v.f.Blocks) {
		out[bi+1] = *st
	}
	return out, nil
}

func mergeEdge(m map[int]State, target int, st State) {
	if prev, ok := m[target]; ok {
		m[target] = mergeStates(prev, st)
	} else {
		m[target] = st
	}
}

// apply updates the state for a non-terminator instruction.
func (v *Verifier) apply(inst ir.Instruction, st *State) error {
	switch inst.Op {
	case ir.OpMov:
		if inst.Src[0].Valid() {
			src, err := v.defined(st, inst.Src[0], inst)
			if err != nil {
				return err
			}
			st[inst.Dst] = *src
			return nil
		}
		st[inst.Dst] = RegState{Defined: true, Type: TypeScalar, Min: inst.Imm, Max: inst.Imm}
		return nil

	case ir.OpArenaCreate:
		if inst.Size <= 0 {
			return v.errf(inst, "arena size must be positive, got %d", inst.Size)
		}
		st[inst.Dst] = RegState{Defined: true, Type: TypeArena, AllocSize: inst.Size}
		return nil

	case ir.OpArenaAlloc:
		arena, err := v.arena(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		if inst.Size <= 0 {
			return v.errf(inst, "allocation size must be positive, got %d", inst.Size)
		}
		if arena.AllocSize > 0 && inst.Size > arena.AllocSize {
			return v.errf(inst, "allocation of %d bytes exceeds arena capacity %d", inst.Size, arena.AllocSize)
		}
		st[inst.Dst] = RegState{
			Defined:   true,
			Type:      TypeDataPtr,
			ArenaID:   inst.Src[0],
			AllocSize: inst.Size,
		}
		return nil

	case ir.OpArenaDestroy:
		rs, err := v.defined(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		if rs.Type != TypeArena {
			return v.errf(inst, "expected arena in %s, got %s", inst.Src[0], rs.Type)
		}
		if rs.Destroyed {
			return v.errf(inst, "arena %s is already destroyed", inst.Src[0])
		}
		rs.Destroyed = true
		for r := range st {
			if st[r].Defined && st[r].Type == TypeDataPtr && st[r].ArenaID == inst.Src[0] {
				st[r].Destroyed = true
			}
		}
		return nil

	case ir.OpLoad:
		ptr, err := v.dataPtr(st, inst.Mem.Base, inst)
		if err != nil {
			return err
		}
		if err := v.checkBounds(st, ptr, inst.Mem, inst); err != nil {
			return err
		}
		st[inst.Dst] = RegState{Defined: true, Type: TypeScalar, Min: math.MinInt64, Max: math.MaxInt64}
		return nil

	case ir.OpStore:
		ptr, err := v.dataPtr(st, inst.Mem.Base, inst)
		if err != nil {
			return err
		}
		if err := v.checkBounds(st, ptr, inst.Mem, inst); err != nil {
			return err
		}
		if _, err := v.scalar(st, inst.Src[0], inst); err != nil {
			return err
		}
		return nil

	case ir.OpAdd, ir.OpSub, ir.OpMul:
		a, err := v.scalar(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		bMin, bMax, err := v.rhsRange(st, inst)
		if err != nil {
			return err
		}
		min, max := combine(inst.Op, a.Min, a.Max, bMin, bMax)
		st[inst.Dst] = RegState{Defined: true, Type: TypeScalar, Min: min, Max: max}
		return nil

	case ir.OpPrint:
		_, err := v.scalar(st, inst.Src[0], inst)
		return err

	case ir.OpCall:
		callee := v.mod.ByName[inst.Label]
		for _, p := range callee.Params {
			rs, err := v.defined(st, p.Reg, inst)
			if err != nil {
				return err
			}
			want := map[ir.ValKind]RegType{
				ir.KindScalar: TypeScalar,
				ir.KindArena:  TypeArena,
				ir.KindPtr:    TypeDataPtr,
			}[p.Kind]
			if rs.Type != want {
				return v.errf(inst, "argument %s to %s: expected %s, got %s",
					p.Reg, callee.Name, want, rs.Type)
			}
			if p.Kind == ir.KindPtr && rs.AllocSize < p.Size {
				return v.errf(inst, "argument %s to %s: %d-byte allocation is smaller than the declared ptr(%d)",
					p.Reg, callee.Name, rs.AllocSize, p.Size)
			}
		}
		// The return value is delivered in r0 with the callee's declared type.
		switch callee.RetKind {
		case ir.KindArena:
			st[0] = RegState{Defined: true, Type: TypeArena}
		case ir.KindPtr:
			st[0] = RegState{Defined: true, Type: TypeDataPtr, ArenaID: ir.NoReg, AllocSize: callee.RetSize}
		default:
			st[0] = RegState{Defined: true, Type: TypeScalar, Min: math.MinInt64, Max: math.MaxInt64}
		}
		return nil

	case ir.OpLabel:
		return nil

	default:
		return v.errf(inst, "unexpected instruction %s", inst.Op)
	}
}

// rhsRange returns the range of the second operand (register or immediate).
func (v *Verifier) rhsRange(st *State, inst ir.Instruction) (int64, int64, error) {
	reg, imm, isReg := inst.RHS()
	if !isReg {
		return imm, imm, nil
	}
	r, err := v.scalar(st, reg, inst)
	if err != nil {
		return 0, 0, err
	}
	return r.Min, r.Max, nil
}

// combine computes the range of a op b for each arithmetic op.
func combine(op ir.OpCode, aMin, aMax, bMin, bMax int64) (int64, int64) {
	switch op {
	case ir.OpAdd:
		return satAdd(aMin, bMin), satAdd(aMax, bMax)
	case ir.OpSub:
		return satSub(aMin, bMax), satSub(aMax, bMin)
	default: // mul
		c1 := satMul(aMin, bMin)
		c2 := satMul(aMin, bMax)
		c3 := satMul(aMax, bMin)
		c4 := satMul(aMax, bMax)
		return min4(c1, c2, c3, c4), max4(c1, c2, c3, c4)
	}
}

// refine narrows the range of the compared register along one branch edge.
func refine(st State, inst ir.Instruction, taken bool) State {
	reg := inst.Src[0]
	rs := st[reg]
	if !rs.Defined || rs.Type != TypeScalar {
		return st
	}
	_, imm, isReg := inst.RHS()
	var bMin, bMax int64
	if isReg {
		bs := st[inst.Src[1]]
		if !bs.Defined || bs.Type != TypeScalar {
			return st
		}
		bMin, bMax = bs.Min, bs.Max
	} else {
		bMin, bMax = imm, imm
	}

	switch inst.Op {
	case ir.OpJeq:
		if taken {
			rs = intersect(rs, bMin, bMax)
		}
	case ir.OpJne:
		if !taken {
			rs = intersect(rs, bMin, bMax)
		}
	case ir.OpJlt: // a < b
		if taken {
			rs.Max = min64(rs.Max, satSub(bMax, 1))
		} else {
			rs.Min = max64(rs.Min, bMin)
		}
	case ir.OpJle: // a <= b
		if taken {
			rs.Max = min64(rs.Max, bMax)
		} else {
			rs.Min = max64(rs.Min, satAdd(bMin, 1))
		}
	case ir.OpJgt: // a > b
		if taken {
			rs.Min = max64(rs.Min, satAdd(bMin, 1))
		} else {
			rs.Max = min64(rs.Max, bMax)
		}
	case ir.OpJge: // a >= b
		if taken {
			rs.Min = max64(rs.Min, bMin)
		} else {
			rs.Max = min64(rs.Max, satSub(bMax, 1))
		}
	}
	st[reg] = rs
	return st
}

func intersect(rs RegState, lo, hi int64) RegState {
	rs.Min = max64(rs.Min, lo)
	rs.Max = min64(rs.Max, hi)
	return rs
}

// checkBounds proves that an access through mem stays within ptr's allocation.
func (v *Verifier) checkBounds(st *State, ptr *RegState, mem ir.Mem, inst ir.Instruction) error {
	var lo, hi int64
	if mem.Index.Valid() {
		idx, err := v.scalar(st, mem.Index, inst)
		if err != nil {
			return err
		}
		lo = satAdd(satMul(idx.Min, mem.Scale), mem.Offset)
		hi = satAdd(satMul(idx.Max, mem.Scale), mem.Offset)
	} else {
		lo, hi = mem.Offset, mem.Offset
	}

	if lo < 0 {
		return v.errf(inst, "out-of-bounds: access %s reaches offset %d (below 0)", mem, lo)
	}
	if hi > ptr.AllocSize-ir.AccessSize {
		return v.errf(inst,
			"out-of-bounds: access %s may reach offset %d, exceeding allocation of %d bytes",
			mem, satAdd(hi, ir.AccessSize), ptr.AllocSize)
	}
	return nil
}

// defined fetches any defined register.
func (v *Verifier) defined(st *State, r ir.Register, inst ir.Instruction) (*RegState, error) {
	if !r.Valid() {
		return nil, v.errf(inst, "missing register operand")
	}
	if !st[r].Defined {
		return nil, v.errf(inst, "use of uninitialized register %s", r)
	}
	return &st[r], nil
}

func (v *Verifier) scalar(st *State, r ir.Register, inst ir.Instruction) (*RegState, error) {
	rs, err := v.defined(st, r, inst)
	if err != nil {
		return nil, err
	}
	if rs.Type != TypeScalar {
		return nil, v.errf(inst, "expected scalar in %s, got %s", r, rs.Type)
	}
	return rs, nil
}

func (v *Verifier) arena(st *State, r ir.Register, inst ir.Instruction) (*RegState, error) {
	rs, err := v.defined(st, r, inst)
	if err != nil {
		return nil, err
	}
	if rs.Type != TypeArena {
		return nil, v.errf(inst, "expected arena in %s, got %s", r, rs.Type)
	}
	if rs.Destroyed {
		return nil, v.errf(inst, "use-after-destroy: arena %s was destroyed", r)
	}
	return rs, nil
}

func (v *Verifier) dataPtr(st *State, r ir.Register, inst ir.Instruction) (*RegState, error) {
	rs, err := v.defined(st, r, inst)
	if err != nil {
		return nil, err
	}
	if rs.Type != TypeDataPtr {
		return nil, v.errf(inst, "expected data pointer in %s, got %s", r, rs.Type)
	}
	if rs.Destroyed {
		return nil, v.errf(inst, "use-after-destroy: %s points into a destroyed arena", r)
	}
	return rs, nil
}

// checkReturn verifies that a RET value matches the function's declared return
// type.
func (v *Verifier) checkReturn(st *State, r ir.Register, inst ir.Instruction) error {
	rs, err := v.defined(st, r, inst)
	if err != nil {
		return err
	}
	if rs.Destroyed {
		return v.errf(inst, "returning %s, which points into a destroyed arena", r)
	}
	want := map[ir.ValKind]RegType{
		ir.KindScalar: TypeScalar,
		ir.KindArena:  TypeArena,
		ir.KindPtr:    TypeDataPtr,
	}[v.f.RetKind]
	if rs.Type != want {
		return v.errf(inst, "returning %s, but function %s declares return type %s",
			rs.Type, v.f.Name, v.f.RetKind)
	}
	if v.f.RetKind == ir.KindPtr && rs.AllocSize < v.f.RetSize {
		return v.errf(inst, "function %s returns ptr(%d), but %s is a %d-byte allocation",
			v.f.Name, v.f.RetSize, r, rs.AllocSize)
	}
	return nil
}

func (v *Verifier) errf(inst ir.Instruction, format string, args ...any) error {
	return &Error{Line: inst.Line, Msg: fmt.Sprintf(format, args...)}
}

// mergeInto merges out into the entry state of target, returning true if the
// state changed (and the block must be reprocessed).
func (v *Verifier) mergeInto(target int, out State) bool {
	cur := v.in[target]
	if cur == nil {
		c := out
		v.in[target] = &c
		return true
	}
	merged := mergeStates(*cur, out)
	if merged == *cur {
		return false
	}
	v.in[target] = &merged
	return true
}

// mergeStates joins two states at a control-flow join. A register is defined
// afterwards only if it is defined on both incoming edges.
func mergeStates(a, b State) State {
	var m State
	for r := 0; r < ir.NumRegs; r++ {
		ra, rb := a[r], b[r]
		if !ra.Defined || !rb.Defined {
			continue
		}
		if ra.Type != rb.Type {
			m[r] = RegState{Defined: true, Type: TypeUnknown}
			continue
		}
		ra.Min = min64(ra.Min, rb.Min)
		ra.Max = max64(ra.Max, rb.Max)
		ra.Destroyed = ra.Destroyed || rb.Destroyed
		ra.AllocSize = minAlloc(ra.AllocSize, rb.AllocSize)
		if ra.Type == TypeDataPtr && ra.ArenaID != rb.ArenaID {
			m[r] = RegState{Defined: true, Type: TypeUnknown}
			continue
		}
		m[r] = ra
	}
	return m
}

func minAlloc(a, b int64) int64 {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	return min64(a, b)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min4(a, b, c, d int64) int64 { return min64(min64(a, b), min64(c, d)) }
func max4(a, b, c, d int64) int64 { return max64(max64(a, b), max64(c, d)) }

func satAdd(a, b int64) int64 {
	s := a + b
	if a > 0 && b > 0 && s < 0 {
		return math.MaxInt64
	}
	if a < 0 && b < 0 && s >= 0 {
		return math.MinInt64
	}
	return s
}

func satSub(a, b int64) int64 {
	s := a - b
	if a >= 0 && b < 0 && s < 0 {
		return math.MaxInt64
	}
	if a < 0 && b > 0 && s >= 0 {
		return math.MinInt64
	}
	return s
}

func satMul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	p := a * b
	if p/b != a {
		if (a > 0) == (b > 0) {
			return math.MaxInt64
		}
		return math.MinInt64
	}
	return p
}
