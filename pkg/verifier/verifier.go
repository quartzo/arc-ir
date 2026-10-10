// Package verifier implements an eBPF-style static verifier for ARC assembly.
//
// It is an abstract interpreter over the control-flow graph. For every program
// point it tracks, for each of the 16 registers, an abstract state:
//
//   - the value type (uninitialized, scalar, arena handle, data pointer),
//   - for scalars, a signed value range [Min, Max],
//   - for arena handles and data pointers, the owning arena, allocation size
//     and allocation identity,
//   - whether the owning arena has been destroyed, and whether this function
//     owns the reference (see the ownership rules below).
//
// It also tracks the type of memory cells written through static offsets, so
// that a stored pointer survives a STORE/LOAD round-trip.
//
// States are propagated along edges and merged at join points until a fixpoint
// is reached. Conditional jumps refine scalar ranges on the taken and
// fall-through edges (e.g. after "JGE r3, 10, exit" the fall-through knows
// r3 < 10). This lets the verifier prove that a dynamic access such as
// [r2 + r3*8] stays inside its allocation.
//
// Rejected programs include: out-of-bounds accesses, use-after-destroy,
// use of an uninitialized register, type misuse, arena leaks, destroying a
// borrowed arena, and any access whose bounds cannot be proven.
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
	// TypeF64 is a 64-bit float (in a vector register).
	TypeF64
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
	case TypeF64:
		return "f64"
	case TypeArena:
		return "arena"
	case TypeDataPtr:
		return "data-ptr"
	default:
		return "unknown"
	}
}

// RegState is the abstract state of one register or memory cell.
type RegState struct {
	Defined   bool
	Type      RegType
	Min, Max  int64 // scalar range
	RefID     int   // arena identity for arena handles and data pointers (-1 = none)
	AllocID   int   // allocation identity for data pointers (-1 = unknown)
	Owned     bool  // this function holds the reference and must release it
	Foreign   bool  // data pointer whose arena provenance is unknown (from a call)
	Growable  bool  // arena handle: extensible (AllocSize is the chunk size)
	AllocSize int64 // reachable bytes (arena capacity/chunk, or allocation size)
	Destroyed bool  // owning arena destroyed / reference released
}

// MemKey identifies a memory cell: an allocation and a byte offset.
type MemKey struct {
	Alloc int
	Off   int64
}

// State is the abstract state of the machine: the register file, the types of
// memory cells written through static offsets, and the bytes allocated so far
// from each arena (keyed by reference id).
type State struct {
	Regs [ir.NumRegs]RegState
	Mem  map[MemKey]RegState
	Used map[int]int64
}

// clone returns a copy that shares no mutable storage with s.
func (s State) clone() State {
	c := State{Regs: s.Regs}
	if len(s.Mem) > 0 {
		c.Mem = make(map[MemKey]RegState, len(s.Mem))
		for k, v := range s.Mem {
			c.Mem[k] = v
		}
	}
	if len(s.Used) > 0 {
		c.Used = make(map[int]int64, len(s.Used))
		for k, v := range s.Used {
			c.Used[k] = v
		}
	}
	return c
}

func (s *State) setCell(alloc int, off int64, c RegState) {
	if s.Mem == nil {
		s.Mem = map[MemKey]RegState{}
	}
	s.Mem[MemKey{alloc, off}] = c
}

func (s *State) getCell(alloc int, off int64) (RegState, bool) {
	c, ok := s.Mem[MemKey{alloc, off}]
	return c, ok
}

func (s *State) getUsed(ref int) int64 {
	return s.Used[ref]
}

func (s *State) setUsed(ref int, n int64) {
	if s.Used == nil {
		s.Used = map[int]int64{}
	}
	s.Used[ref] = n
}

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
		v.assignIDs()
		entry, err := v.entryState()
		if err != nil {
			return nil, err
		}
		v.in[0] = &entry
		work := []int{0}

		for len(work) > 0 {
			bi := work[0]
			work = work[1:]

			st := v.in[bi].clone()
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
	mod          *ir.Module
	f            *ir.Function
	in           []*State
	refOf        map[[2]int]int // (block, instr) -> arena RefID
	allocOf      map[[2]int]int // (block, instr) -> allocation AllocID
	nextParamRef int
}

// assignIDs gives every arena-producing and allocation instruction a stable
// identity. Ids are derived from instruction position so they do not change
// across fixpoint iterations.
func (v *Verifier) assignIDs() {
	v.refOf = map[[2]int]int{}
	v.allocOf = map[[2]int]int{}
	id := 0
	for bi, b := range v.f.Blocks {
		for ii, inst := range b.Instrs {
			key := [2]int{bi, ii}
			switch inst.Op {
			case ir.OpArenaCreate:
				v.refOf[key] = id
				id++
			case ir.OpArenaAlloc:
				v.allocOf[key] = id
				id++
			case ir.OpCall:
				if c := v.mod.ByName[inst.Label]; c != nil && c.RetKind == ir.KindArena {
					v.refOf[key] = id
					id++
				}
			}
		}
	}
	v.nextParamRef = id
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
			st.Regs[p.Reg] = RegState{Defined: true, Type: TypeScalar, RefID: -1, AllocID: -1, Min: math.MinInt64, Max: math.MaxInt64}
		case ir.KindArena:
			// Capacity is unknown unless declared as arena(<size>); allocations
			// are then bounds-checked and capacity-checked. A parameter is
			// borrowed unless declared "own".
			st.Regs[p.Reg] = RegState{Defined: true, Type: TypeArena, RefID: v.nextParamRef, AllocID: -1, Owned: p.Own, AllocSize: p.Size}
			v.nextParamRef++
		case ir.KindPtr:
			// Provenance is unknown; treat it as possibly aliasing any arena.
			st.Regs[p.Reg] = RegState{Defined: true, Type: TypeDataPtr, RefID: -1, AllocID: -1, Foreign: true, AllocSize: p.Size}
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

	for ii, inst := range b.Instrs {
		switch {
		case inst.Op == ir.OpRet:
			if err := v.checkReturn(st, inst); err != nil {
				return nil, err
			}
			return out, nil

		case inst.Op == ir.OpJmp:
			out[v.f.Index[inst.Label]] = st.clone()
			return out, nil

		case inst.Op.IsCondJump():
			if _, err := v.scalar(st, inst.Src[0], inst); err != nil {
				return nil, err
			}
			taken := refine(*st, inst, true)
			mergeEdge(out, v.f.Index[inst.Label], taken)
			*st = refine(*st, inst, false)

		default:
			if err := v.apply(inst, st, v.refOf[[2]int{bi, ii}], v.allocOf[[2]int{bi, ii}]); err != nil {
				return nil, err
			}
		}
	}

	// Fall through to the next block, if any.
	if bi+1 < len(v.f.Blocks) {
		out[bi+1] = st.clone()
	}
	return out, nil
}

func mergeEdge(m map[int]State, target int, st State) {
	if prev, ok := m[target]; ok {
		m[target] = mergeStates(prev, st)
	} else {
		m[target] = st.clone()
	}
}

// apply updates the state for a non-terminator instruction. ref and alloc are
// the identities assigned to arena-producing / allocating instructions.
func (v *Verifier) apply(inst ir.Instruction, st *State, ref, alloc int) error {
	if inst.Op.HasDest() && !inst.Dst.Valid() {
		return v.errf(inst, "%s requires a destination register", inst.Op)
	}
	switch inst.Op {
	case ir.OpMov:
		if inst.Src[0].Valid() {
			src, err := v.defined(st, inst.Src[0], inst)
			if err != nil {
				return err
			}
			st.Regs[inst.Dst] = *src
			return nil
		}
		st.Regs[inst.Dst] = RegState{Defined: true, Type: TypeScalar, RefID: -1, AllocID: -1, Min: inst.Imm, Max: inst.Imm}
		return nil

	case ir.OpArenaCreate:
		if inst.Size <= 0 {
			return v.errf(inst, "arena size must be positive, got %d", inst.Size)
		}
		st.Regs[inst.Dst] = RegState{Defined: true, Type: TypeArena, RefID: ref, AllocID: -1, Owned: true, Growable: inst.Growable, AllocSize: inst.Size}
		st.setUsed(ref, 0)
		return nil

	case ir.OpArenaAlloc:
		arena, err := v.arena(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		if inst.Size <= 0 {
			return v.errf(inst, "allocation size must be positive, got %d", inst.Size)
		}
		if arena.Growable {
			// Each allocation must fit in a single chunk; the arena grows.
			if arena.AllocSize > 0 && inst.Size > arena.AllocSize {
				return v.errf(inst, "allocation of %d bytes exceeds chunk size %d", inst.Size, arena.AllocSize)
			}
		} else {
			used := st.getUsed(arena.RefID)
			if arena.AllocSize > 0 && satAdd(used, inst.Size) > arena.AllocSize {
				return v.errf(inst,
					"arena exhausted: %d bytes already allocated + %d requested exceeds capacity %d",
					used, inst.Size, arena.AllocSize)
			}
			st.setUsed(arena.RefID, satAdd(used, inst.Size))
		}
		st.Regs[inst.Dst] = RegState{
			Defined:   true,
			Type:      TypeDataPtr,
			RefID:     arena.RefID,
			AllocID:   alloc,
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
		if !rs.Owned {
			return v.errf(inst, "cannot destroy borrowed arena %s (not owned here)", inst.Src[0])
		}
		v.release(st, rs.RefID)
		return nil

	case ir.OpLoad:
		ptr, err := v.dataPtr(st, inst.Mem.Base, inst)
		if err != nil {
			return err
		}
		if err := v.checkBounds(st, ptr, inst.Mem, inst); err != nil {
			return err
		}
		if inst.Mem.Index.Valid() || ptr.AllocID < 0 {
			st.Regs[inst.Dst] = unknownScalar()
			return nil
		}
		if cell, ok := st.getCell(ptr.AllocID, inst.Mem.Offset); ok && cell.Type == TypeDataPtr {
			st.Regs[inst.Dst] = RegState{
				Defined:   true,
				Type:      TypeDataPtr,
				RefID:     cell.RefID,
				AllocID:   cell.AllocID,
				AllocSize: cell.AllocSize,
				Destroyed: cell.Destroyed,
			}
			return nil
		}
		st.Regs[inst.Dst] = unknownScalar()
		return nil

	case ir.OpStore:
		ptr, err := v.dataPtr(st, inst.Mem.Base, inst)
		if err != nil {
			return err
		}
		if err := v.checkBounds(st, ptr, inst.Mem, inst); err != nil {
			return err
		}
		src, err := v.defined(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		// Only static offsets into a locally-known allocation are typed.
		if inst.Mem.Index.Valid() || ptr.AllocID < 0 {
			if src.Type != TypeScalar {
				return v.errf(inst, "only scalars may be stored at a dynamic or foreign offset")
			}
			return nil
		}
		switch src.Type {
		case TypeScalar:
			st.setCell(ptr.AllocID, inst.Mem.Offset, RegState{Type: TypeScalar, RefID: -1, AllocID: -1})
		case TypeDataPtr:
			st.setCell(ptr.AllocID, inst.Mem.Offset, RegState{
				Type:      TypeDataPtr,
				RefID:     src.RefID,
				AllocID:   src.AllocID,
				AllocSize: src.AllocSize,
				Destroyed: src.Destroyed,
			})
		default:
			return v.errf(inst, "cannot store a value of type %s in memory", src.Type)
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
		st.Regs[inst.Dst] = RegState{Defined: true, Type: TypeScalar, RefID: -1, AllocID: -1, Min: min, Max: max}
		return nil

	case ir.OpFConst:
		if !inst.Dst.IsVec() {
			return v.errf(inst, "a float result requires a vector register")
		}
		st.Regs[inst.Dst] = RegState{Defined: true, Type: TypeF64, RefID: -1, AllocID: -1}
		return nil

	case ir.OpFAdd, ir.OpFSub, ir.OpFMul, ir.OpFDiv:
		if !inst.Dst.IsVec() {
			return v.errf(inst, "a float result requires a vector register")
		}
		if _, err := v.float(st, inst.Src[0], inst); err != nil {
			return err
		}
		if _, err := v.float(st, inst.Src[1], inst); err != nil {
			return err
		}
		st.Regs[inst.Dst] = RegState{Defined: true, Type: TypeF64, RefID: -1, AllocID: -1}
		return nil

	case ir.OpIToF:
		if !inst.Dst.IsVec() {
			return v.errf(inst, "a float result requires a vector register")
		}
		if _, err := v.scalar(st, inst.Src[0], inst); err != nil {
			return err
		}
		st.Regs[inst.Dst] = RegState{Defined: true, Type: TypeF64, RefID: -1, AllocID: -1}
		return nil

	case ir.OpFToI:
		if !inst.Dst.IsGP() {
			return v.errf(inst, "FTOI result requires a general register")
		}
		if _, err := v.float(st, inst.Src[0], inst); err != nil {
			return err
		}
		st.Regs[inst.Dst] = unknownScalar()
		return nil

	case ir.OpPrintF:
		_, err := v.float(st, inst.Src[0], inst)
		return err

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
			switch p.Kind {
			case ir.KindPtr:
				if rs.AllocSize < p.Size {
					return v.errf(inst, "argument %s to %s: %d-byte allocation is smaller than the declared ptr(%d)",
						p.Reg, callee.Name, rs.AllocSize, p.Size)
				}
			case ir.KindArena:
				if p.Size > 0 && !rs.Growable && rs.AllocSize > 0 && rs.AllocSize < p.Size {
					return v.errf(inst, "argument %s to %s: arena capacity %d is smaller than the declared %d",
						p.Reg, callee.Name, rs.AllocSize, p.Size)
				}
				if p.Own {
					if !rs.Owned {
						return v.errf(inst, "argument %s to %s: cannot transfer a borrowed arena",
							p.Reg, callee.Name)
					}
					v.release(st, rs.RefID)
				}
			}
		}
		// The return value is delivered in r0 with the callee's declared type.
		switch callee.RetKind {
		case ir.KindArena:
			st.Regs[0] = RegState{Defined: true, Type: TypeArena, RefID: ref, AllocID: -1, Owned: true, AllocSize: callee.RetSize}
		case ir.KindPtr:
			st.Regs[0] = RegState{Defined: true, Type: TypeDataPtr, RefID: -1, AllocID: -1, Foreign: true, AllocSize: callee.RetSize}
		default:
			st.Regs[0] = unknownScalar()
		}
		return nil

	case ir.OpLabel:
		return nil

	default:
		return v.errf(inst, "unexpected instruction %s", inst.Op)
	}
}

func unknownScalar() RegState {
	return RegState{Defined: true, Type: TypeScalar, RefID: -1, AllocID: -1, Min: math.MinInt64, Max: math.MaxInt64}
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
	rs := st.Regs[reg]
	if !rs.Defined || rs.Type != TypeScalar {
		return st
	}
	_, imm, isReg := inst.RHS()
	var bMin, bMax int64
	if isReg {
		bs := st.Regs[inst.Src[1]]
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
	st.Regs[reg] = rs
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
	if !st.Regs[r].Defined {
		return nil, v.errf(inst, "use of uninitialized register %s", r)
	}
	return &st.Regs[r], nil
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

func (v *Verifier) float(st *State, r ir.Register, inst ir.Instruction) (*RegState, error) {
	rs, err := v.defined(st, r, inst)
	if err != nil {
		return nil, err
	}
	if rs.Type != TypeF64 {
		return nil, v.errf(inst, "expected f64 in %s, got %s", r, rs.Type)
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

// release frees the reference identified by refID: every handle, pointer and
// memory cell referring to that arena becomes invalid. Pointers of unknown
// provenance (Foreign) are invalidated too, since they may alias it.
func (v *Verifier) release(st *State, refID int) {
	for r := range st.Regs {
		rs := &st.Regs[r]
		if !rs.Defined || (rs.Type != TypeArena && rs.Type != TypeDataPtr) {
			continue
		}
		if rs.RefID == refID || rs.Foreign {
			rs.Destroyed = true
			rs.Owned = false
		}
	}
	for k, c := range st.Mem {
		if (c.Type == TypeArena || c.Type == TypeDataPtr) && (c.RefID == refID || c.Foreign) {
			c.Destroyed = true
			st.Mem[k] = c
		}
	}
	delete(st.Used, refID)
}

// checkReturn verifies a RET: the returned value matches the declared return
// type, and every owned arena has been released or is being returned.
func (v *Verifier) checkReturn(st *State, inst ir.Instruction) error {
	returnedRef := -1
	if inst.Src[0].Valid() {
		rs, err := v.defined(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		if rs.Destroyed {
			return v.errf(inst, "returning %s, which is no longer live", inst.Src[0])
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
		switch v.f.RetKind {
		case ir.KindArena:
			if !rs.Owned {
				return v.errf(inst, "function %s cannot return borrowed arena %s", v.f.Name, inst.Src[0])
			}
			returnedRef = rs.RefID
		case ir.KindPtr:
			if rs.AllocSize < v.f.RetSize {
				return v.errf(inst, "function %s returns ptr(%d), but %s is a %d-byte allocation",
					v.f.Name, v.f.RetSize, inst.Src[0], rs.AllocSize)
			}
		}
	} else if v.f.RetKind != ir.KindScalar {
		return v.errf(inst, "function %s returns %s but RET has no value", v.f.Name, v.f.RetKind)
	}

	// Every owned arena must be released or returned: no leaks.
	for r := 0; r < ir.NumRegs; r++ {
		rs := st.Regs[r]
		if rs.Defined && rs.Owned && rs.Type == TypeArena && !rs.Destroyed && rs.RefID != returnedRef {
			return v.errf(inst, "arena %s is never released (leaked)", ir.Register(r))
		}
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
		c := out.clone()
		v.in[target] = &c
		return true
	}
	merged := mergeStates(*cur, out)
	if stateEqual(merged, *cur) {
		return false
	}
	v.in[target] = &merged
	return true
}

func stateEqual(a, b State) bool {
	if a.Regs != b.Regs {
		return false
	}
	if len(a.Mem) != len(b.Mem) {
		return false
	}
	for k, v := range a.Mem {
		if w, ok := b.Mem[k]; !ok || w != v {
			return false
		}
	}
	if len(a.Used) != len(b.Used) {
		return false
	}
	for k, v := range a.Used {
		if w, ok := b.Used[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// mergeStates joins two states at a control-flow join. A register or cell is
// kept only if it is defined on both incoming edges.
func mergeStates(a, b State) State {
	var m State
	for r := 0; r < ir.NumRegs; r++ {
		ra, rb := a.Regs[r], b.Regs[r]
		if !ra.Defined || !rb.Defined {
			continue
		}
		if ra.Type != rb.Type {
			m.Regs[r] = RegState{Defined: true, Type: TypeUnknown, RefID: -1, AllocID: -1}
			continue
		}
		ra.Min = min64(ra.Min, rb.Min)
		ra.Max = max64(ra.Max, rb.Max)
		ra.Destroyed = ra.Destroyed || rb.Destroyed
		ra.Foreign = ra.Foreign || rb.Foreign
		ra.Growable = ra.Growable && rb.Growable
		ra.AllocSize = minAlloc(ra.AllocSize, rb.AllocSize)
		if ra.RefID != rb.RefID || ra.AllocID != rb.AllocID {
			// Different arenas/allocations on the two paths: provenance and
			// ownership are unknown.
			ra.RefID = -1
			ra.AllocID = -1
			ra.Owned = false
		} else {
			ra.Owned = ra.Owned && rb.Owned
		}
		m.Regs[r] = ra
	}
	m.Mem = mergeMem(a.Mem, b.Mem)
	m.Used = mergeUsed(a.Used, b.Used)
	return m
}

// mergeUsed joins the per-arena allocated-bytes maps conservatively: a key
// present on either path is kept, taking the larger value, so the check at a
// later allocation is at least as strict as on any path.
func mergeUsed(a, b map[int]int64) map[int]int64 {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[int]int64, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if w, ok := out[k]; !ok || v > w {
			out[k] = v
		}
	}
	return out
}

func mergeMem(a, b map[MemKey]RegState) map[MemKey]RegState {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	out := make(map[MemKey]RegState, len(a))
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			continue
		}
		if va.Type != vb.Type {
			out[k] = RegState{Type: TypeUnknown, RefID: -1, AllocID: -1}
			continue
		}
		va.Destroyed = va.Destroyed || vb.Destroyed
		va.Foreign = va.Foreign || vb.Foreign
		va.AllocSize = minAlloc(va.AllocSize, vb.AllocSize)
		if va.RefID != vb.RefID || va.AllocID != vb.AllocID {
			va.RefID = -1
			va.AllocID = -1
		}
		out[k] = va
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
