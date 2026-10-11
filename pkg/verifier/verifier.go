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
	// TypeI64 is a signed 64-bit integer (the default scalar type).
	TypeI64
	// TypeI8, TypeI16, TypeI32 are signed sub-word integers.
	TypeI8
	TypeI16
	TypeI32
	// TypeU8, TypeU16, TypeU32, TypeU64 are unsigned integers.
	TypeU8
	TypeU16
	TypeU32
	TypeU64
	// TypeF32 is a 32-bit float (in a vector register).
	TypeF32
	// TypeF64 is a 64-bit float (in a vector register).
	TypeF64
	// TypeF32x4 is a 128-bit vector of four f32.
	TypeF32x4
	// TypeF64x2 is a 128-bit vector of two f64.
	TypeF64x2
	// TypeI32x4 is a 128-bit vector of four i32.
	TypeI32x4
	// TypeI64x2 is a 128-bit vector of two i64.
	TypeI64x2
	// TypeArena is an arena handle.
	TypeArena
	// TypeDataPtr is a pointer into an arena allocation.
	TypeDataPtr
)

// String returns a human-readable type name.
func (t RegType) String() string {
	switch t {
	case TypeI64:
		return "i64"
	case TypeI8:
		return "i8"
	case TypeI16:
		return "i16"
	case TypeI32:
		return "i32"
	case TypeU8:
		return "u8"
	case TypeU16:
		return "u16"
	case TypeU32:
		return "u32"
	case TypeU64:
		return "u64"
	case TypeF32:
		return "f32"
	case TypeF64:
		return "f64"
	case TypeF32x4:
		return "f32x4"
	case TypeF64x2:
		return "f64x2"
	case TypeI32x4:
		return "i32x4"
	case TypeI64x2:
		return "i64x2"
	case TypeArena:
		return "arena"
	case TypeDataPtr:
		return "data-ptr"
	default:
		return "unknown"
	}
}

// isInt reports whether t is an integer type.
func isInt(t RegType) bool {
	switch t {
	case TypeI8, TypeI16, TypeI32, TypeI64, TypeU8, TypeU16, TypeU32, TypeU64:
		return true
	}
	return false
}

// isFloat reports whether t is a scalar float type.
func isFloat(t RegType) bool { return t == TypeF32 || t == TypeF64 }

// width returns the access width of a scalar type in bytes.
func width(t RegType) int64 {
	switch t {
	case TypeI8, TypeU8:
		return 1
	case TypeI16, TypeU16:
		return 2
	case TypeI32, TypeU32, TypeF32:
		return 4
	default:
		return 8
	}
}

// isSigned reports whether an integer type is signed.
func isSigned(t RegType) bool {
	switch t {
	case TypeI8, TypeI16, TypeI32, TypeI64:
		return true
	}
	return false
}

// scalarRegType maps an IR scalar kind to its abstract register type.
func scalarRegType(k ir.ScalarKind) RegType {
	switch k {
	case ir.I8:
		return TypeI8
	case ir.I16:
		return TypeI16
	case ir.I32:
		return TypeI32
	case ir.U8:
		return TypeU8
	case ir.U16:
		return TypeU16
	case ir.U32:
		return TypeU32
	case ir.U64:
		return TypeU64
	case ir.F32:
		return TypeF32
	case ir.F64:
		return TypeF64
	default:
		return TypeI64
	}
}

// intRange returns the value range of an integer type. For u64 the range is the
// whole signed domain, which is exactly the set of 64-bit patterns (sound).
func intRange(t RegType) (int64, int64) {
	switch t {
	case TypeI8:
		return -128, 127
	case TypeI16:
		return -32768, 32767
	case TypeI32:
		return math.MinInt32, math.MaxInt32
	case TypeU8:
		return 0, 255
	case TypeU16:
		return 0, 65535
	case TypeU32:
		return 0, math.MaxUint32
	default:
		return math.MinInt64, math.MaxInt64
	}
}

// RegState is the abstract state of one register or memory cell.
type RegState struct {
	Defined   bool
	Type      RegType
	Min, Max  int64         // scalar range
	RefID     int           // arena identity for arena handles and data pointers (-1 = unknown, may alias any arena)
	AllocID   int           // allocation identity for data pointers (-1 = unknown)
	Owned     bool          // this function holds the reference and must release it
	Foreign   bool          // data pointer whose arena provenance is unknown (from a call)
	Growable  bool          // arena handle: extensible (AllocSize is the chunk size)
	Struct    string        // data pointer: struct or enum type name, if any
	Elem      ir.ScalarKind // data pointer: array element type (when Count > 0)
	Count     int64         // data pointer: array element count
	Variant   int           // enum pointer: active variant index (-1 = unknown)
	AllocSize int64         // reachable bytes (arena capacity/chunk, or allocation size)
	Init      bool          // memory cell: has been written
	Declared  bool          // memory cell: seeded from a struct field (type is a contract)
	Clobbered bool          // register destroyed by a call (a scratch register the callee writes)
	Destroyed bool          // owning arena destroyed / reference released
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

// clearAlloc drops every typed cell of an allocation and forgets any enum
// variant tracked for it, used when a callee may have written through a
// pointer to it.
func (s *State) clearAlloc(alloc int) {
	for k := range s.Mem {
		if k.Alloc == alloc {
			delete(s.Mem, k)
		}
	}
	for r := range s.Regs {
		if s.Regs[r].AllocID == alloc {
			s.Regs[r].Variant = -1
		}
	}
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
	Blocks int        // total basic blocks verified
	Funcs  int        // number of functions verified
	Module *ir.Module // the verified module, with field accesses lowered
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

	res := &Result{Funcs: len(m.Funcs), Module: m}
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
	mod            *ir.Module
	f              *ir.Function
	in             []*State
	refOf          map[[2]int]int // (block, instr) -> arena RefID
	allocOf        map[[2]int]int // (block, instr) -> allocation AllocID
	nextParamRef   int
	nextParamAlloc int
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
	v.nextParamAlloc = id
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
			t := scalarRegType(p.Scalar)
			lo, hi := intRange(t)
			st.Regs[p.Reg] = RegState{Defined: true, Type: t, RefID: -1, AllocID: -1, Min: lo, Max: hi}
		case ir.KindArena:
			// Capacity is unknown unless declared as arena(<size>); allocations
			// are then bounds-checked and capacity-checked. A parameter is
			// borrowed unless declared "own".
			st.Regs[p.Reg] = RegState{Defined: true, Type: TypeArena, RefID: v.nextParamRef, AllocID: -1, Owned: p.Own, AllocSize: p.Size}
			v.nextParamRef++
		case ir.KindPtr:
			// Provenance is unknown; treat it as possibly aliasing any arena.
			// The parameter gets its own allocation view so its cells can be
			// typed inside the callee.
			alloc := v.nextParamAlloc
			v.nextParamAlloc++
			st.Regs[p.Reg] = RegState{Defined: true, Type: TypeDataPtr, RefID: -1, AllocID: alloc, Foreign: true, AllocSize: p.Size, Struct: p.Struct, Elem: p.Elem, Count: p.Count, Variant: -1}
			if p.Struct != "" {
				if err := v.seedNamed(&st, alloc, p.Struct, true); err != nil {
					return st, fmt.Errorf("function %s: %w", v.f.Name, err)
				}
			}
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
			if inst.Op == ir.OpJvariant {
				if err := v.jvariant(st, out, inst); err != nil {
					return nil, err
				}
				break
			}
			rs, err := v.scalar(st, inst.Src[0], inst)
			if err != nil {
				return nil, err
			}
			// Lower a signed comparison to its unsigned form when the operand
			// type is unsigned, so codegen emits the right condition code.
			inst = unsignedCompare(inst, rs.Type)
			b.Instrs[ii] = inst
			taken := refine(*st, inst, true)
			mergeEdge(out, v.f.Index[inst.Label], taken)
			*st = refine(*st, inst, false)

		default:
			resolved, err := v.resolve(inst, st)
			if err != nil {
				return nil, err
			}
			if err := v.annotate(st, &resolved); err != nil {
				return nil, err
			}
			if err := v.apply(resolved, st, v.refOf[[2]int{bi, ii}], v.allocOf[[2]int{bi, ii}]); err != nil {
				return nil, err
			}
			b.Instrs[ii] = resolved
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

// resolve lowers a field access to a concrete load/store at a static offset.
// The rewritten instruction is written back into the verified module, so the
// code generator sees only plain LOAD/STORE.
func (v *Verifier) resolve(inst ir.Instruction, st *State) (ir.Instruction, error) {
	if inst.Op != ir.OpField && inst.Op != ir.OpFieldStore {
		return inst, nil
	}
	base, err := v.defined(st, inst.Src[0], inst)
	if err != nil {
		return inst, err
	}
	if base.Type != TypeDataPtr || base.Struct == "" {
		return inst, v.errf(inst, "field access needs a struct or enum pointer in %s", inst.Src[0])
	}
	off := int64(-1)
	switch {
	case v.mod.Structs[base.Struct] != nil:
		off, _ = v.mod.Structs[base.Struct].Offset(inst.Field)
	case v.mod.Enums[base.Struct] != nil:
		e := v.mod.Enums[base.Struct]
		if base.Variant < 0 {
			return inst, v.errf(inst, "enum %s has an unknown active variant; test it with JV first", base.Struct)
		}
		vf := e.Variants[base.Variant].Fields
		for i := range vf {
			if vf[i].Name == inst.Field {
				off = vf[i].Offset
				break
			}
		}
	default:
		return inst, v.errf(inst, "unknown type %s", base.Struct)
	}
	if off < 0 {
		return inst, v.errf(inst, "%s has no field %q", base.Struct, inst.Field)
	}
	out := ir.Instruction{Line: inst.Line}
	out.Src = [2]ir.Register{ir.NoReg, ir.NoReg}
	out.Mem = ir.Mem{Base: inst.Src[0], Index: ir.NoReg, Offset: off}
	if inst.Op == ir.OpField {
		out.Op = ir.OpLoad
		out.Dst = inst.Dst
	} else {
		out.Op = ir.OpStore
		out.Src[0] = inst.Src[1]
	}
	return out, nil
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
		st.Regs[inst.Dst] = RegState{Defined: true, Type: TypeI64, RefID: -1, AllocID: -1, Min: inst.Imm, Max: inst.Imm}
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
			Struct:    inst.Struct,
			Elem:      inst.Elem,
			Count:     inst.Count,
			Variant:   -1,
			AllocSize: inst.Size,
		}
		if inst.Struct != "" {
			if err := v.seedNamed(st, alloc, inst.Struct, false); err != nil {
				return v.errf(inst, "%s", err)
			}
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
		rt, _, err := v.accessType(st, ptr, inst.Mem, inst)
		if err != nil {
			return err
		}
		if err := v.checkBounds(st, ptr, inst.Mem, inst.Width, inst); err != nil {
			return err
		}
		if isFloat(rt) {
			if !inst.Dst.IsVec() {
				return v.errf(inst, "loading %s requires a vector register", rt)
			}
			st.Regs[inst.Dst] = RegState{Defined: true, Type: rt, RefID: -1, AllocID: -1}
			return nil
		}
		if !inst.Dst.IsGP() {
			return v.errf(inst, "loading %s requires a general register", rt)
		}
		// A declared struct or enum field keeps the exact stored type and value.
		if ptr.Count == 0 {
			if v.mod.Enums[ptr.Struct] != nil {
				f := v.enumFieldAt(ptr.Struct, ptr.Variant, inst.Mem.Offset)
				cell, ok := st.getCell(ptr.AllocID, inst.Mem.Offset)
				if !ok || !cell.Init {
					return v.errf(inst, "field at offset %d is not initialized", inst.Mem.Offset)
				}
				if f != nil && cell.Type == fieldRegType(f) {
					st.Regs[inst.Dst] = loadFromCell(cell)
				} else if f != nil {
					st.Regs[inst.Dst] = fieldState(f)
				} else {
					st.Regs[inst.Dst] = loadFromCell(cell)
				}
				return nil
			}
			if cell, ok := st.getCell(ptr.AllocID, inst.Mem.Offset); ok {
				if cell.Declared && !cell.Init {
					return v.errf(inst, "field at offset %d is not initialized", inst.Mem.Offset)
				}
				st.Regs[inst.Dst] = loadFromCell(cell)
				return nil
			}
		}
		lo, hi := intRange(rt)
		st.Regs[inst.Dst] = RegState{Defined: true, Type: rt, RefID: -1, AllocID: -1, Min: lo, Max: hi}
		return nil

	case ir.OpStore:
		ptr, err := v.dataPtr(st, inst.Mem.Base, inst)
		if err != nil {
			return err
		}
		src, err := v.defined(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		rt, _, err := v.accessType(st, ptr, inst.Mem, inst)
		if err != nil {
			return err
		}
		if err := v.checkBounds(st, ptr, inst.Mem, inst.Width, inst); err != nil {
			return err
		}
		if ptr.Count > 0 {
			// Array element: a float element needs the matching float type; an
			// integer element accepts any integer (the store truncates).
			if isFloat(rt) {
				if src.Type != rt {
					return v.errf(inst, "array element expects %s, got %s", rt, src.Type)
				}
				if !inst.Src[0].IsVec() {
					return v.errf(inst, "storing %s requires a vector register", rt)
				}
			} else {
				if !isInt(src.Type) {
					return v.errf(inst, "array element expects an integer, got %s", src.Type)
				}
				if !inst.Src[0].IsGP() {
					return v.errf(inst, "storing an integer requires a general register")
				}
			}
			return nil
		}
		// Struct or enum: the field's declared cell (tracking initialization)
		// or, for a foreign pointer, the field type from the layout.
		var cell RegState
		found := false
		if v.mod.Enums[ptr.Struct] != nil {
			if f := v.enumFieldAt(ptr.Struct, ptr.Variant, inst.Mem.Offset); f != nil {
				cell, found = fieldCell(f), true
			}
		} else if c, ok := st.getCell(ptr.AllocID, inst.Mem.Offset); ok && c.Declared {
			cell, found = c, true
		} else if f := structFieldAt(v.mod, ptr.Struct, inst.Mem.Offset); f != nil {
			cell, found = fieldCell(f), true
		}
		if !found {
			return v.errf(inst, "no field at offset %d in %s", inst.Mem.Offset, ptr.Struct)
		}
		if err := v.fieldCompatible(cell, src, inst); err != nil {
			return err
		}
		st.setCell(ptr.AllocID, inst.Mem.Offset, RegState{
			Type:      cell.Type,
			RefID:     src.RefID,
			AllocID:   src.AllocID,
			AllocSize: cell.AllocSize,
			Struct:    cell.Struct,
			Elem:      cell.Elem,
			Count:     cell.Count,
			Declared:  true,
			Init:      true,
			Destroyed: src.Destroyed,
		})
		return nil

	case ir.OpVariant:
		ptr, err := v.enumPtr(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		e := v.mod.Enums[ptr.Struct]
		if inst.Variant < 0 || inst.Variant >= len(e.Variants) {
			return v.errf(inst, "invalid variant of enum %s", ptr.Struct)
		}
		// The tag is now known; the payload starts uninitialized.
		st.setCell(ptr.AllocID, 0, RegState{Type: TypeU8, Declared: true, Init: true})
		v.clearPayload(st, ptr.AllocID, e.PayloadOff())
		ptr.Variant = inst.Variant
		return nil

	case ir.OpAdd, ir.OpSub, ir.OpMul:
		if !inst.Dst.IsGP() {
			return v.errf(inst, "an integer result requires a general register")
		}
		a, err := v.scalar(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		bMin, bMax, err := v.rhsRange(st, inst)
		if err != nil {
			return err
		}
		min, max := combine(inst.Op, a.Min, a.Max, bMin, bMax)
		st.Regs[inst.Dst] = RegState{Defined: true, Type: TypeI64, RefID: -1, AllocID: -1, Min: min, Max: max}
		return nil

	case ir.OpFConst:
		if !inst.Dst.IsVec() {
			return v.errf(inst, "a float result requires a vector register")
		}
		ft := TypeF64
		if inst.F32 {
			ft = TypeF32
		}
		st.Regs[inst.Dst] = RegState{Defined: true, Type: ft, RefID: -1, AllocID: -1}
		return nil

	case ir.OpFAdd, ir.OpFSub, ir.OpFMul, ir.OpFDiv:
		return v.floatBin(st, inst)

	case ir.OpIToF:
		return v.iToF(st, inst, TypeF64)
	case ir.OpIToF32:
		return v.iToF(st, inst, TypeF32)

	case ir.OpFToI:
		return v.fToI(st, inst)

	case ir.OpPrintF:
		a, err := v.defined(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		if !isFloat(a.Type) {
			return v.errf(inst, "expected a float in %s, got %s", inst.Src[0], a.Type)
		}
		return nil

	case ir.OpVConst:
		if !inst.Dst.IsVec() {
			return v.errf(inst, "a vector result requires a vector register")
		}
		st.Regs[inst.Dst] = RegState{Defined: true, Type: vecType(inst.Vec), RefID: -1, AllocID: -1}
		return nil
	case ir.OpVAdd, ir.OpVSub, ir.OpVMul, ir.OpVDiv:
		if !inst.Dst.IsVec() {
			return v.errf(inst, "a vector result requires a vector register")
		}
		a, err := v.defined(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		if !isVecType(a.Type) {
			return v.errf(inst, "expected a vector in %s, got %s", inst.Src[0], a.Type)
		}
		vt := a.Type
		if vt == TypeI32x4 || vt == TypeI64x2 {
			if inst.Op == ir.OpVDiv {
				return v.errf(inst, "packed integer division is not available")
			}
			if inst.Op == ir.OpVMul && vt == TypeI64x2 {
				return v.errf(inst, "packed 64-bit multiply is not available")
			}
		}
		if _, err := v.vecOf(st, inst.Src[1], vt, inst); err != nil {
			return err
		}
		st.Regs[inst.Dst] = RegState{Defined: true, Type: vt, RefID: -1, AllocID: -1}
		return nil
	case ir.OpPrintV:
		a, err := v.defined(st, inst.Src[0], inst)
		if err != nil {
			return err
		}
		if !isVecType(a.Type) {
			return v.errf(inst, "expected a vector in %s, got %s", inst.Src[0], a.Type)
		}
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
			switch p.Kind {
			case ir.KindScalar:
				if !isInt(rs.Type) {
					return v.errf(inst, "argument %s to %s: expected an integer, got %s", p.Reg, callee.Name, rs.Type)
				}
			case ir.KindArena:
				if rs.Type != TypeArena {
					return v.errf(inst, "argument %s to %s: expected arena, got %s", p.Reg, callee.Name, rs.Type)
				}
			case ir.KindPtr:
				if rs.Type != TypeDataPtr {
					return v.errf(inst, "argument %s to %s: expected a data pointer, got %s", p.Reg, callee.Name, rs.Type)
				}
			}
			switch p.Kind {
			case ir.KindPtr:
				if p.Struct != "" {
					if rs.Struct != p.Struct {
						return v.errf(inst, "argument %s to %s: expected a %s pointer", p.Reg, callee.Name, p.Struct)
					}
					if rs.AllocID >= 0 {
						if sf := v.mod.Structs[p.Struct]; sf != nil {
							for i := range sf.Fields {
								f := &sf.Fields[i]
								cell, ok := st.getCell(rs.AllocID, f.Offset)
								if !ok || !cell.Init {
									return v.errf(inst, "argument %s to %s: field %s is not initialized",
										p.Reg, callee.Name, f.Name)
								}
							}
						}
					}
				} else if rs.AllocSize < p.Size {
					return v.errf(inst, "argument %s to %s: %d-byte allocation is smaller than the required %d bytes",
						p.Reg, callee.Name, rs.AllocSize, p.Size)
				}
				// The callee may write through the pointer, so the caller's
				// view of the pointed-to cells becomes stale.
				if rs.AllocID >= 0 {
					st.clearAlloc(rs.AllocID)
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
		// A call destroys every scratch register: scratch is volatile. Vector
		// registers are volatile too. The preserved registers survive (the callee
		// saves the ones it writes).
		for r := 0; r < ir.NumScratch; r++ {
			if ir.Register(r) != 0 {
				st.Regs[r] = RegState{Clobbered: true}
			}
		}
		for r := ir.NumGP; r < ir.NumRegs; r++ {
			st.Regs[r] = RegState{Clobbered: true}
		}
		// The return value is delivered in r0 with the callee's declared type.
		switch callee.RetKind {
		case ir.KindArena:
			st.Regs[0] = RegState{Defined: true, Type: TypeArena, RefID: ref, AllocID: -1, Owned: true, AllocSize: callee.RetSize}
		case ir.KindPtr:
			st.Regs[0] = RegState{Defined: true, Type: TypeDataPtr, RefID: -1, AllocID: -1, Foreign: true, AllocSize: callee.RetSize, Struct: callee.RetStruct, Elem: callee.RetElem, Count: callee.RetCount, Variant: -1}
		default:
			t := scalarRegType(callee.RetScalar)
			lo, hi := intRange(t)
			st.Regs[0] = RegState{Defined: true, Type: t, RefID: -1, AllocID: -1, Min: lo, Max: hi}
		}
		return nil

	case ir.OpLabel:
		return nil

	default:
		return v.errf(inst, "unexpected instruction %s", inst.Op)
	}
}

func unknownScalar() RegState {
	return RegState{Defined: true, Type: TypeI64, RefID: -1, AllocID: -1, Min: math.MinInt64, Max: math.MaxInt64}
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
	if !rs.Defined || !isInt(rs.Type) {
		return st
	}
	_, imm, isReg := inst.RHS()
	var bMin, bMax int64
	if isReg {
		bs := st.Regs[inst.Src[1]]
		if !bs.Defined || !isInt(bs.Type) {
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
	case ir.OpJlt, ir.OpJltu: // a < b
		if taken {
			rs.Max = min64(rs.Max, satSub(bMax, 1))
		} else {
			rs.Min = max64(rs.Min, bMin)
		}
	case ir.OpJle, ir.OpJleu: // a <= b
		if taken {
			rs.Max = min64(rs.Max, bMax)
		} else {
			rs.Min = max64(rs.Min, satAdd(bMin, 1))
		}
	case ir.OpJgt, ir.OpJgtu: // a > b
		if taken {
			rs.Min = max64(rs.Min, satAdd(bMin, 1))
		} else {
			rs.Max = min64(rs.Max, bMax)
		}
	case ir.OpJge, ir.OpJgeu: // a >= b
		if taken {
			rs.Min = max64(rs.Min, bMin)
		} else {
			rs.Max = min64(rs.Max, satSub(bMax, 1))
		}
	}
	st.Regs[reg] = rs
	return st
}

// unsignedCompare rewrites a signed comparison to its unsigned form when the
// operand type is unsigned.
func unsignedCompare(inst ir.Instruction, t RegType) ir.Instruction {
	if isSigned(t) {
		return inst
	}
	switch inst.Op {
	case ir.OpJlt:
		inst.Op = ir.OpJltu
	case ir.OpJle:
		inst.Op = ir.OpJleu
	case ir.OpJgt:
		inst.Op = ir.OpJgtu
	case ir.OpJge:
		inst.Op = ir.OpJgeu
	}
	return inst
}

func intersect(rs RegState, lo, hi int64) RegState {
	rs.Min = max64(rs.Min, lo)
	rs.Max = min64(rs.Max, hi)
	return rs
}

// checkBounds proves that an access of width w through mem stays within ptr's
// allocation.
func (v *Verifier) checkBounds(st *State, ptr *RegState, mem ir.Mem, w int64, inst ir.Instruction) error {
	if w <= 0 {
		w = 1
	}
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
	if hi > ptr.AllocSize-w {
		return v.errf(inst,
			"out-of-bounds: access %s may reach offset %d, exceeding allocation of %d bytes",
			mem, satAdd(hi, w), ptr.AllocSize)
	}
	return nil
}

// defined fetches any defined register.
func (v *Verifier) defined(st *State, r ir.Register, inst ir.Instruction) (*RegState, error) {
	if !r.Valid() {
		return nil, v.errf(inst, "missing register operand")
	}
	if st.Regs[r].Clobbered {
		return nil, v.errf(inst, "use of destroyed register %s (a call destroyed it)", r)
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
	if !isInt(rs.Type) {
		return nil, v.errf(inst, "expected integer in %s, got %s", r, rs.Type)
	}
	return rs, nil
}

func (v *Verifier) floatOf(st *State, r ir.Register, ft RegType, inst ir.Instruction) (*RegState, error) {
	rs, err := v.defined(st, r, inst)
	if err != nil {
		return nil, err
	}
	if rs.Type != ft {
		return nil, v.errf(inst, "expected %s in %s, got %s", ft, r, rs.Type)
	}
	return rs, nil
}

func vecType(k ir.VecKind) RegType {
	switch k {
	case ir.VecF64x2:
		return TypeF64x2
	case ir.VecI32x4:
		return TypeI32x4
	case ir.VecI64x2:
		return TypeI64x2
	default:
		return TypeF32x4
	}
}

// vecKindOf maps a vector register type back to its IR kind.
func vecKindOf(t RegType) ir.VecKind {
	switch t {
	case TypeF64x2:
		return ir.VecF64x2
	case TypeI32x4:
		return ir.VecI32x4
	case TypeI64x2:
		return ir.VecI64x2
	default:
		return ir.VecF32x4
	}
}

// isVecType reports whether t is a SIMD vector type.
func isVecType(t RegType) bool {
	switch t {
	case TypeF32x4, TypeF64x2, TypeI32x4, TypeI64x2:
		return true
	}
	return false
}

func (v *Verifier) vecOf(st *State, r ir.Register, want RegType, inst ir.Instruction) (*RegState, error) {
	rs, err := v.defined(st, r, inst)
	if err != nil {
		return nil, err
	}
	if rs.Type != want {
		return nil, v.errf(inst, "expected %s in %s, got %s", want, r, rs.Type)
	}
	return rs, nil
}

func fieldRegType(f *ir.Field) RegType {
	switch f.Type {
	case ir.KindArena:
		return TypeArena
	case ir.KindPtr:
		return TypeDataPtr
	default:
		return scalarRegType(f.Scalar)
	}
}

func loadFromCell(cell RegState) RegState {
	out := RegState{
		Defined:   true,
		Type:      cell.Type,
		RefID:     cell.RefID,
		AllocID:   cell.AllocID,
		AllocSize: cell.AllocSize,
		Struct:    cell.Struct,
		Elem:      cell.Elem,
		Count:     cell.Count,
		Destroyed: cell.Destroyed,
	}
	if isInt(cell.Type) {
		out.Min, out.Max = intRange(cell.Type)
	}
	return out
}

func (v *Verifier) fieldCompatible(cell RegState, src *RegState, inst ir.Instruction) error {
	switch {
	case cell.Type == TypeDataPtr:
		if src.Type != TypeDataPtr {
			return v.errf(inst, "field expects a data pointer, got %s", src.Type)
		}
		if src.AllocSize < cell.AllocSize {
			return v.errf(inst, "field expects at least %d bytes, got a %d-byte allocation", cell.AllocSize, src.AllocSize)
		}
	case cell.Type == TypeArena:
		if src.Type != TypeArena {
			return v.errf(inst, "field expects an arena, got %s", src.Type)
		}
	case isInt(cell.Type):
		if !isInt(src.Type) {
			return v.errf(inst, "field expects an integer, got %s", src.Type)
		}
	default: // float
		if src.Type != cell.Type {
			return v.errf(inst, "field expects %s, got %s", cell.Type, src.Type)
		}
	}
	return nil
}

// structFieldAt returns the field of the named struct whose offset is off.
func structFieldAt(mod *ir.Module, name string, off int64) *ir.Field {
	sf := mod.Structs[name]
	if sf == nil {
		return nil
	}
	for i := range sf.Fields {
		if sf.Fields[i].Offset == off {
			return &sf.Fields[i]
		}
	}
	return nil
}

// fieldCell builds the abstract cell for a struct or variant field.
func fieldCell(f *ir.Field) RegState {
	return RegState{
		Type:      fieldRegType(f),
		RefID:     -1,
		AllocID:   -1,
		AllocSize: f.Size,
		Struct:    f.Struct,
		Elem:      f.Elem,
		Count:     f.Count,
		Declared:  true,
	}
}

// fieldState builds a fresh value of a field's type (used when reading a field
// whose cell only records initialization, e.g. an enum payload).
func fieldState(f *ir.Field) RegState {
	rt := fieldRegType(f)
	out := RegState{Defined: true, Type: rt, RefID: -1, AllocID: -1, AllocSize: f.Size, Struct: f.Struct, Elem: f.Elem, Count: f.Count}
	if isInt(rt) {
		out.Min, out.Max = intRange(rt)
	}
	return out
}

// seedNamed seeds the cells of a struct or enum region. A struct seeds every
// field; an enum seeds only the tag cell (its payload is uninitialized until a
// variant is selected).
func (v *Verifier) seedNamed(st *State, alloc int, name string, init bool) error {
	if sf := v.mod.Structs[name]; sf != nil {
		for i := range sf.Fields {
			c := fieldCell(&sf.Fields[i])
			c.Init = init
			st.setCell(alloc, sf.Fields[i].Offset, c)
		}
		return nil
	}
	if v.mod.Enums[name] != nil {
		st.setCell(alloc, 0, RegState{Type: TypeU8, Declared: true, Init: init})
		if init {
			// For a parameter the caller guarantees the active variant's fields
			// are initialized. Seed one init cell per distinct payload offset;
			// the concrete type comes from the active variant at read time.
			seen := map[int64]bool{}
			for vi := range v.mod.Enums[name].Variants {
				for fi := range v.mod.Enums[name].Variants[vi].Fields {
					f := &v.mod.Enums[name].Variants[vi].Fields[fi]
					if seen[f.Offset] {
						continue
					}
					seen[f.Offset] = true
					st.setCell(alloc, f.Offset, RegState{Type: TypeUnknown, Declared: true, Init: true})
				}
			}
		}
		return nil
	}
	return fmt.Errorf("unknown type %q", name)
}

// enumFieldAt returns the field of an enum's active variant at offset off.
func (v *Verifier) enumFieldAt(enumName string, variant int, off int64) *ir.Field {
	e := v.mod.Enums[enumName]
	if e == nil {
		return nil
	}
	return e.VariantFieldAt(variant, off)
}

// accessType returns the abstract type and byte width of an access through ptr.
func (v *Verifier) accessType(st *State, ptr *RegState, mem ir.Mem, inst ir.Instruction) (RegType, int64, error) {
	if ptr.Count > 0 {
		rt := scalarRegType(ptr.Elem)
		return rt, ptr.Elem.Size(), nil
	}
	if ptr.Struct != "" {
		if mem.Index.Valid() {
			return 0, 0, v.errf(inst, "a dynamic offset into a struct or enum pointer is not allowed; use a field")
		}
		if v.mod.Enums[ptr.Struct] != nil {
			if ptr.Variant < 0 {
				return 0, 0, v.errf(inst, "enum %s has an unknown active variant; test it with JV first", ptr.Struct)
			}
			f := v.enumFieldAt(ptr.Struct, ptr.Variant, mem.Offset)
			if f == nil {
				return 0, 0, v.errf(inst, "no field at offset %d in the active variant of %s", mem.Offset, ptr.Struct)
			}
			if cell, ok := st.getCell(ptr.AllocID, mem.Offset); ok && cell.Init && cell.Type != TypeUnknown {
				return cell.Type, width(cell.Type), nil
			}
			return fieldRegType(f), f.Width(), nil
		}
		if cell, ok := st.getCell(ptr.AllocID, mem.Offset); ok {
			return cell.Type, width(cell.Type), nil
		}
		if f := structFieldAt(v.mod, ptr.Struct, mem.Offset); f != nil {
			return fieldRegType(f), f.Width(), nil
		}
		return 0, 0, v.errf(inst, "no field at offset %d in %s", mem.Offset, ptr.Struct)
	}
	return 0, 0, v.errf(inst, "pointer has no type; cannot determine the access width")
}

// annotate records the concrete width (load/store, float ops) or vector kind
// (SIMD ops) that the instruction needs, so the code generator can emit the
// right machine instruction.
func (v *Verifier) annotate(st *State, inst *ir.Instruction) error {
	switch inst.Op {
	case ir.OpLoad, ir.OpStore:
		ptr, err := v.dataPtr(st, inst.Mem.Base, *inst)
		if err != nil {
			return err
		}
		rt, w, err := v.accessType(st, ptr, inst.Mem, *inst)
		if err != nil {
			return err
		}
		inst.Width = w
		inst.Signed = isSigned(rt)
	case ir.OpFAdd, ir.OpFSub, ir.OpFMul, ir.OpFDiv, ir.OpFToI, ir.OpPrintF:
		if rs := st.Regs[inst.Src[0]]; rs.Defined && isFloat(rs.Type) {
			inst.Width = width(rs.Type)
		}
	case ir.OpVAdd, ir.OpVSub, ir.OpVMul, ir.OpVDiv, ir.OpPrintV:
		if rs := st.Regs[inst.Src[0]]; rs.Defined && isVecType(rs.Type) {
			inst.Vec = vecKindOf(rs.Type)
		}
	}
	return nil
}

func (v *Verifier) floatBin(st *State, inst ir.Instruction) error {
	if !inst.Dst.IsVec() {
		return v.errf(inst, "a float result requires a vector register")
	}
	a, err := v.defined(st, inst.Src[0], inst)
	if err != nil {
		return err
	}
	if !isFloat(a.Type) {
		return v.errf(inst, "expected a float in %s, got %s", inst.Src[0], a.Type)
	}
	if _, err := v.floatOf(st, inst.Src[1], a.Type, inst); err != nil {
		return err
	}
	st.Regs[inst.Dst] = RegState{Defined: true, Type: a.Type, RefID: -1, AllocID: -1}
	return nil
}

func (v *Verifier) iToF(st *State, inst ir.Instruction, ft RegType) error {
	if !inst.Dst.IsVec() {
		return v.errf(inst, "a float result requires a vector register")
	}
	if _, err := v.scalar(st, inst.Src[0], inst); err != nil {
		return err
	}
	st.Regs[inst.Dst] = RegState{Defined: true, Type: ft, RefID: -1, AllocID: -1}
	return nil
}

func (v *Verifier) fToI(st *State, inst ir.Instruction) error {
	if !inst.Dst.IsGP() {
		return v.errf(inst, "%s result requires a general register", inst.Op)
	}
	a, err := v.defined(st, inst.Src[0], inst)
	if err != nil {
		return err
	}
	if !isFloat(a.Type) {
		return v.errf(inst, "expected a float in %s, got %s", inst.Src[0], a.Type)
	}
	st.Regs[inst.Dst] = unknownScalar()
	return nil
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

// enumPtr fetches a register that holds a pointer to an enum.
func (v *Verifier) enumPtr(st *State, r ir.Register, inst ir.Instruction) (*RegState, error) {
	rs, err := v.dataPtr(st, r, inst)
	if err != nil {
		return nil, err
	}
	if rs.Struct == "" || v.mod.Enums[rs.Struct] == nil {
		return nil, v.errf(inst, "expected an enum pointer in %s", r)
	}
	return rs, nil
}

// jvariant refines the active variant along a JV branch: the taken edge knows
// the variant; the fall-through knows it is not that variant. A test of a
// variant already known to be inactive can never be taken.
func (v *Verifier) jvariant(st *State, out map[int]State, inst ir.Instruction) error {
	ptr, err := v.enumPtr(st, inst.Src[0], inst)
	if err != nil {
		return err
	}
	e := v.mod.Enums[ptr.Struct]
	if inst.Variant < 0 || inst.Variant >= len(e.Variants) {
		return v.errf(inst, "invalid variant of enum %s", ptr.Struct)
	}
	if ptr.Variant < 0 || ptr.Variant == inst.Variant {
		taken := st.clone()
		taken.Regs[inst.Src[0]].Variant = inst.Variant
		mergeEdge(out, v.f.Index[inst.Label], taken)
	}
	if ptr.Variant == inst.Variant {
		ptr.Variant = -1
	}
	return nil
}

// clearPayload drops the payload cells of an enum allocation, used when a new
// variant is selected so the old payload cannot be read.
func (v *Verifier) clearPayload(st *State, alloc int, payloadOff int64) {
	for k := range st.Mem {
		if k.Alloc == alloc && k.Off >= payloadOff {
			delete(st.Mem, k)
		}
	}
}

// release frees the reference identified by refID: every handle, pointer and
// memory cell referring to that arena becomes invalid. A reference of unknown
// provenance — Foreign (from a call), or RefID == -1 (a merge that lost the
// arena identity, or a pointer allocated from an unknown handle) — is
// invalidated too, because it may alias the released arena. Anything else would
// let a pointer whose arena identity was lost survive its arena's release.
func (v *Verifier) release(st *State, refID int) {
	for r := range st.Regs {
		rs := &st.Regs[r]
		if !rs.Defined || (rs.Type != TypeArena && rs.Type != TypeDataPtr) {
			continue
		}
		if rs.RefID == refID || rs.RefID == -1 || rs.Foreign {
			rs.Destroyed = true
			rs.Owned = false
		}
	}
	for k, c := range st.Mem {
		if (c.Type == TypeArena || c.Type == TypeDataPtr) &&
			(c.RefID == refID || c.RefID == -1 || c.Foreign) {
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
		switch v.f.RetKind {
		case ir.KindScalar:
			if !isInt(rs.Type) {
				return v.errf(inst, "returning %s, but function %s declares return type %s",
					rs.Type, v.f.Name, v.f.RetScalar)
			}
		case ir.KindArena:
			if rs.Type != TypeArena {
				return v.errf(inst, "returning %s, but function %s declares return type %s",
					rs.Type, v.f.Name, v.f.RetKind)
			}
			if !rs.Owned {
				return v.errf(inst, "function %s cannot return borrowed arena %s", v.f.Name, inst.Src[0])
			}
			returnedRef = rs.RefID
		case ir.KindPtr:
			if rs.Type != TypeDataPtr {
				return v.errf(inst, "returning %s, but function %s declares return type %s",
					rs.Type, v.f.Name, v.f.RetKind)
			}
			if v.f.RetStruct != "" && rs.Struct != v.f.RetStruct {
				return v.errf(inst, "function %s must return a %s pointer", v.f.Name, v.f.RetStruct)
			}
			if rs.AllocSize < v.f.RetSize {
				return v.errf(inst, "function %s returns %d bytes, but %s is a %d-byte allocation",
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
		if ra.Struct != rb.Struct || ra.Count != rb.Count || ra.Elem != rb.Elem {
			ra.Struct = ""
			ra.Count = 0
		}
		if ra.Variant != rb.Variant {
			ra.Variant = -1
		}
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
