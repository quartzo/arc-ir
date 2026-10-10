// Package ir defines the ARC flat-register assembly IR.
//
// Unlike an SSA IR, ARC models a real machine: two register files that
// instructions read and write in place — 16 general-purpose registers
// (r0..r15) and 16 vector registers (v0..v15, mapped to XMM) — plus direct
// jumps between labeled blocks. There are no virtual registers, no phi nodes,
// and no basic-block value merging — the control flow is exactly the control
// flow of the emitted machine code.
//
// The verifier (pkg/verifier) is therefore an eBPF-style abstract interpreter:
// it walks the instruction stream, tracks the type, arena binding and value
// range of every register at every program point, and merges states at jump
// targets.
package ir

import (
	"fmt"
	"math"
	"strings"
)

// Register identifies one of the machine registers. Registers 0..15 are the
// general-purpose file (r0..r15); registers 16..31 are the vector file
// (v0..v15, mapped to XMM registers), which hold floats and SIMD values.
type Register int

const (
	// NumGP is the number of general-purpose registers.
	NumGP = 16
	// NumVec is the number of vector registers.
	NumVec = 16
	// NumRegs is the total size of the register file.
	NumRegs = NumGP + NumVec
)

// NoReg is the sentinel for "no register" (e.g. an absent index or operand).
const NoReg Register = NumRegs

// AccessSize is the width, in bytes, of a scalar load/store.
const AccessSize int64 = 8

// Valid reports whether r names a real register.
func (r Register) Valid() bool { return r >= 0 && r < NumRegs }

// IsGP reports whether r is a general-purpose register.
func (r Register) IsGP() bool { return r >= 0 && r < NumGP }

// IsVec reports whether r is a vector register.
func (r Register) IsVec() bool { return r >= NumGP && r < NumRegs }

// String renders a register as "rN" or "vN".
func (r Register) String() string {
	switch {
	case r.IsGP():
		return fmt.Sprintf("r%d", int(r))
	case r.IsVec():
		return fmt.Sprintf("v%d", int(r)-NumGP)
	default:
		return "<none>"
	}
}

// ValKind is the declared type of a function parameter.
type ValKind int

const (
	// KindScalar is a scalar (integer or float) parameter. The concrete type is
	// in the Scalar field.
	KindScalar ValKind = iota
	// KindArena is an arena-handle parameter.
	KindArena
	// KindPtr is a data-pointer parameter.
	KindPtr
)

var kindNames = map[ValKind]string{
	KindScalar: "value",
	KindArena:  "arena",
	KindPtr:    "ptr",
}

// String returns the textual name of the parameter kind.
func (k ValKind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return "unknown"
}

// VecKind is the element type of a SIMD vector.
type VecKind int

const (
	// VecF32x4 is four 32-bit floats.
	VecF32x4 VecKind = iota
	// VecF64x2 is two 64-bit floats.
	VecF64x2
	// VecI32x4 is four 32-bit integers.
	VecI32x4
	// VecI64x2 is two 64-bit integers.
	VecI64x2
)

var vecKindNames = map[VecKind]string{
	VecF32x4: "f32x4",
	VecF64x2: "f64x2",
	VecI32x4: "i32x4",
	VecI64x2: "i64x2",
}

// String returns the textual name of the vector kind.
func (k VecKind) String() string {
	if n, ok := vecKindNames[k]; ok {
		return n
	}
	return "vec(?)"
}

// ParseVecKind resolves a textual vector kind.
func ParseVecKind(s string) (VecKind, bool) {
	for k, n := range vecKindNames {
		if n == s {
			return k, true
		}
	}
	return VecF32x4, false
}

// ScalarKind is an integer or float scalar type. Integer types carry a width
// (1, 2, 4 or 8 bytes) and a signedness, which together select the machine
// load/store and comparison instructions.
type ScalarKind int

const (
	// I64 is the default scalar type (the zero value), an alias for "scalar".
	I64 ScalarKind = iota
	I8
	I16
	I32
	U8
	U16
	U32
	U64
	F32
	F64
)

var scalarNames = map[ScalarKind]string{
	I8: "i8", I16: "i16", I32: "i32", I64: "i64",
	U8: "u8", U16: "u16", U32: "u32", U64: "u64",
	F32: "f32", F64: "f64",
}

// String returns the textual name of the scalar kind.
func (k ScalarKind) String() string {
	if n, ok := scalarNames[k]; ok {
		return n
	}
	return "scalar(?)"
}

// ParseScalarKind resolves a textual scalar type.
func ParseScalarKind(s string) (ScalarKind, bool) {
	for k, n := range scalarNames {
		if n == s {
			return k, true
		}
	}
	return I64, false
}

// Size returns the width of the scalar in bytes.
func (k ScalarKind) Size() int64 {
	switch k {
	case I8, U8:
		return 1
	case I16, U16:
		return 2
	case I32, U32, F32:
		return 4
	default:
		return 8
	}
}

// Signed reports whether the integer type is signed. Floats return false.
func (k ScalarKind) Signed() bool {
	switch k {
	case I8, I16, I32, I64:
		return true
	}
	return false
}

// Float reports whether the type is a floating-point type.
func (k ScalarKind) Float() bool { return k == F32 || k == F64 }

// IsInt reports whether the type is an integer (not a float).
func (k ScalarKind) IsInt() bool { return !k.Float() }

// Param is a declared function parameter: a register and its type. For
// KindPtr parameters, Size is the size of the allocation the pointer refers
// to, which the callee's bounds checks rely on. Own marks an arena parameter
// whose ownership is transferred to the callee; otherwise it is borrowed.
type Param struct {
	Reg    Register
	Kind   ValKind
	Scalar ScalarKind // KindScalar: the integer/float type
	Size   int64      // KindPtr: pointed-to region size in bytes
	Struct string     // KindPtr: ptr(Struct)
	Elem   ScalarKind // KindPtr: ptr([Elem; Count])
	Count  int64      // KindPtr: ptr([Elem; Count])
	Own    bool
}

// OpCode enumerates the ARC instruction set.
type OpCode int

const (
	OpInvalid OpCode = iota
	OpLabel          // label:  (pseudo-instruction; defines Label)
	OpFunc           // func name(params):  (pseudo-instruction; starts a function)

	OpMov          // rdst = imm            | rdst = rsrc
	OpArenaCreate  // rdst = ARENA_CREATE tier, size
	OpArenaAlloc   // rdst = ARENA_ALLOC rsrc, size
	OpArenaDestroy // ARENA_DESTROY rsrc
	OpLoad         // rdst = LOAD [mem]
	OpStore        // STORE [mem], rsrc
	OpField        // rdst = rbase.field      (resolved by the verifier to LOAD)
	OpFieldStore   // rbase.field = rsrc      (resolved by the verifier to STORE)
	OpAdd          // rdst = ADD ra, (rb | imm)
	OpSub          // rdst = SUB ra, (rb | imm)
	OpMul          // rdst = MUL ra, (rb | imm)
	OpFConst       // vdst = <float literal>       (f64, or f32 when F32 is set)
	OpFAdd         // vdst = FADD vsrc, vsrc       (f32 or f64 by operand type)
	OpFSub         // vdst = FSUB vsrc, vsrc       (f32 or f64 by operand type)
	OpFMul         // vdst = FMUL vsrc, vsrc       (f32 or f64 by operand type)
	OpFDiv         // vdst = FDIV vsrc, vsrc       (f32 or f64 by operand type)
	OpIToF         // vdst = ITOF rsrc   (integer to f64)
	OpIToF32       // vdst = ITOF32 rsrc (integer to f32)
	OpFToI         // rdst = FTOI vsrc   (f32 or f64 to integer)
	OpPrint        // PRINT rsrc
	OpPrintF       // PRINTF vsrc        (f32 or f64 by operand type)
	OpVConst       // vdst = f32x4(...) | i32x4(...)
	OpVAdd         // vdst = VADD vsrc, vsrc  (kind from operand type)
	OpVSub         // vdst = VSUB vsrc, vsrc  (kind from operand type)
	OpVMul         // vdst = VMUL vsrc, vsrc  (kind from operand type)
	OpVDiv         // vdst = VDIV vsrc, vsrc  (kind from operand type)
	OpPrintV       // PRINTV vsrc
	OpRet          // RET rsrc
	OpCall         // CALL name

	OpJmp  // JMP label
	OpJeq  // JEQ ra, (rb | imm), label
	OpJne  // JNE ra, (rb | imm), label
	OpJlt  // JLT ra, (rb | imm), label   (signed)
	OpJle  // JLE ra, (rb | imm), label   (signed)
	OpJgt  // JGT ra, (rb | imm), label   (signed)
	OpJge  // JGE ra, (rb | imm), label   (signed)
	OpJltu // JLTU ra, (rb | imm), label  (unsigned)
	OpJleu // JLEU ra, (rb | imm), label  (unsigned)
	OpJgtu // JGTU ra, (rb | imm), label  (unsigned)
	OpJgeu // JGEU ra, (rb | imm), label  (unsigned)
)

var opNames = map[OpCode]string{
	OpInvalid:      "invalid",
	OpLabel:        "label",
	OpFunc:         "func",
	OpMov:          "MOV",
	OpArenaCreate:  "ARENA_CREATE",
	OpArenaAlloc:   "ARENA_ALLOC",
	OpArenaDestroy: "ARENA_DESTROY",
	OpLoad:         "LOAD",
	OpStore:        "STORE",
	OpField:        "FIELD",
	OpFieldStore:   "FIELDSTORE",
	OpAdd:          "ADD",
	OpSub:          "SUB",
	OpMul:          "MUL",
	OpFConst:       "FCONST",
	OpFAdd:         "FADD",
	OpFSub:         "FSUB",
	OpFMul:         "FMUL",
	OpFDiv:         "FDIV",
	OpIToF:         "ITOF",
	OpIToF32:       "ITOF32",
	OpFToI:         "FTOI",
	OpPrint:        "PRINT",
	OpPrintF:       "PRINTF",
	OpVConst:       "VCONST",
	OpVAdd:         "VADD",
	OpVSub:         "VSUB",
	OpVMul:         "VMUL",
	OpVDiv:         "VDIV",
	OpPrintV:       "PRINTV",
	OpRet:          "RET",
	OpCall:         "CALL",
	OpJmp:          "JMP",
	OpJeq:          "JEQ",
	OpJne:          "JNE",
	OpJlt:          "JLT",
	OpJle:          "JLE",
	OpJgt:          "JGT",
	OpJge:          "JGE",
	OpJltu:         "JLTU",
	OpJleu:         "JLEU",
	OpJgtu:         "JGTU",
	OpJgeu:         "JGEU",
}

// String returns the mnemonic of the opcode.
func (o OpCode) String() string {
	if n, ok := opNames[o]; ok {
		return n
	}
	return "OP(?)"
}

// OpFromName resolves a mnemonic to an OpCode.
func OpFromName(name string) (OpCode, bool) {
	for op, n := range opNames {
		if n == name {
			return op, true
		}
	}
	return OpInvalid, false
}

// IsCondJump reports whether the opcode is a conditional jump.
func (o OpCode) IsCondJump() bool {
	switch o {
	case OpJeq, OpJne, OpJlt, OpJle, OpJgt, OpJge, OpJltu, OpJleu, OpJgtu, OpJgeu:
		return true
	default:
		return false
	}
}

// IsTerminator reports whether the opcode unconditionally ends a basic block.
// Conditional jumps are not terminators: control falls through to the next
// instruction as well as branching to the label.
func (o OpCode) IsTerminator() bool {
	return o == OpJmp || o == OpRet
}

// HasDest reports whether the opcode writes Dst.
func (o OpCode) HasDest() bool {
	switch o {
	case OpMov, OpArenaCreate, OpArenaAlloc, OpLoad, OpField, OpAdd, OpSub, OpMul,
		OpFConst, OpFAdd, OpFSub, OpFMul, OpFDiv,
		OpIToF, OpIToF32, OpFToI,
		OpVConst, OpVAdd, OpVSub, OpVMul, OpVDiv:
		return true
	default:
		return false
	}
}

// Mem is an x86-style memory operand: base + index*scale + offset.
type Mem struct {
	Base   Register
	Index  Register // NoReg when absent
	Scale  int64    // 1, 2, 4 or 8 (only when Index is set)
	Offset int64
}

// String renders the operand in "[base + index*scale + offset]" form.
func (m Mem) String() string {
	s := m.Base.String()
	if m.Index.Valid() {
		s += fmt.Sprintf(" + %s*%d", m.Index, m.Scale)
	}
	if m.Offset != 0 || (!m.Index.Valid() && m.Base.Valid()) {
		if m.Offset >= 0 {
			s += fmt.Sprintf(" + %d", m.Offset)
		} else {
			s += fmt.Sprintf(" - %d", -m.Offset)
		}
	}
	return "[" + s + "]"
}

// Instruction is a single ARC operation.
//
// Field usage by opcode:
//
//	OpMov          Dst, Imm            (immediate)   or Dst, Src[0] (register)
//	OpArenaCreate  Dst, Tier, Size
//	OpArenaAlloc   Dst, Src[0]=arena, Size
//	OpArenaDestroy Src[0]=arena
//	OpLoad         Dst, Mem
//	OpStore        Mem, Src[0]=value
//	OpAdd/Sub/Mul  Dst, Src[0], and Src[1] (register) or Imm (immediate)
//	OpPrint        Src[0]
//	OpRet          Src[0] (optional)
//	OpJmp          Label
//	Jcc            Src[0], and Src[1] or Imm, Label
type Instruction struct {
	Op       OpCode
	Dst      Register
	Src      [2]Register
	Imm      int64
	FImm     float64 // OpFConst: the float literal
	F32      bool    // OpFConst: the literal is an f32
	Vec      VecKind // OpVConst / VADD..: the vector element type
	Lanes    []int64 // OpVConst: lane values (32-bit each)
	Mem      Mem
	Label    string
	Tier     TopologyTier
	Size     int64
	Growable bool       // OpArenaCreate: extensible arena; Size is the chunk size
	Struct   string     // OpArenaAlloc: struct type name
	Elem     ScalarKind // OpArenaAlloc: array element type (when Count > 0)
	Count    int64      // OpArenaAlloc: array element count
	Field    string     // OpField / OpFieldStore: field name

	// Width and Signed describe a LOAD/STORE access; the verifier fills them
	// from the type at the access (1, 2, 4 or 8 bytes).
	Width  int64
	Signed bool

	// Params holds a function's declared parameters (OpFunc only).
	Params []Param
	// RetKind/RetSize hold a function's declared return type (OpFunc only).
	// The zero value (KindScalar, 0) means a scalar return.
	RetKind   ValKind
	RetScalar ScalarKind
	RetSize   int64
	RetStruct string
	RetElem   ScalarKind
	RetCount  int64

	// Line is the 1-based source line, attached by the parser.
	Line int
}

// RHS returns the second operand of a binary op / conditional jump. When isReg
// is false the operand is the immediate Imm.
func (i Instruction) RHS() (reg Register, imm int64, isReg bool) {
	if i.Src[1].Valid() {
		return i.Src[1], 0, true
	}
	return NoReg, i.Imm, false
}

// String renders the instruction in ARC text form (used in diagnostics).
func (i Instruction) String() string {
	dst := ""
	if i.Dst.Valid() {
		dst = i.Dst.String() + " = "
	}
	switch i.Op {
	case OpLabel:
		return i.Label + ":"
	case OpFunc:
		return fmt.Sprintf("func %s(%s)%s:", i.Label, formatParams(i.Params), formatRet(i))
	case OpCall:
		return fmt.Sprintf("CALL %s", i.Label)
	case OpMov:
		if i.Src[0].Valid() {
			return dst + i.Src[0].String()
		}
		return fmt.Sprintf("%s%d", dst, i.Imm)
	case OpArenaCreate:
		if i.Growable {
			return fmt.Sprintf("%sARENA_CREATE %s, growable(%d)", dst, i.Tier, i.Size)
		}
		return fmt.Sprintf("%sARENA_CREATE %s, %d", dst, i.Tier, i.Size)
	case OpArenaAlloc:
		switch {
		case i.Struct != "":
			return fmt.Sprintf("%sARENA_ALLOC %s, %s", dst, i.Src[0], i.Struct)
		case i.Count > 0:
			return fmt.Sprintf("%sARENA_ALLOC %s, [%s; %d]", dst, i.Src[0], i.Elem, i.Count)
		default:
			return fmt.Sprintf("%sARENA_ALLOC %s, %d", dst, i.Src[0], i.Size)
		}
	case OpArenaDestroy:
		return fmt.Sprintf("ARENA_DESTROY %s", i.Src[0])
	case OpLoad:
		return fmt.Sprintf("%sLOAD %s", dst, i.Mem)
	case OpStore:
		return fmt.Sprintf("STORE %s, %s", i.Mem, i.Src[0])
	case OpAdd, OpSub, OpMul:
		rhs := fmt.Sprintf("%d", i.Imm)
		if i.Src[1].Valid() {
			rhs = i.Src[1].String()
		}
		return fmt.Sprintf("%s%s %s, %s", dst, i.Op, i.Src[0], rhs)
	case OpFConst:
		if i.F32 {
			return fmt.Sprintf("%sf32(%v)", dst, i.FImm)
		}
		return fmt.Sprintf("%s%v", dst, i.FImm)
	case OpFAdd, OpFSub, OpFMul, OpFDiv:
		return fmt.Sprintf("%s%s %s, %s", dst, i.Op, i.Src[0], i.Src[1])
	case OpIToF, OpIToF32, OpFToI:
		return fmt.Sprintf("%s%s %s", dst, i.Op, i.Src[0])
	case OpPrintF:
		return fmt.Sprintf("%s %s", i.Op, i.Src[0])
	case OpVConst:
		return fmt.Sprintf("%s%s(%s)", dst, i.Vec, formatLanes(i))
	case OpVAdd, OpVSub, OpVMul, OpVDiv:
		return fmt.Sprintf("%s%s %s, %s", dst, i.Op, i.Src[0], i.Src[1])
	case OpPrintV:
		return fmt.Sprintf("PRINTV %s", i.Src[0])
	case OpPrint:
		return fmt.Sprintf("PRINT %s", i.Src[0])
	case OpRet:
		if i.Src[0].Valid() {
			return fmt.Sprintf("RET %s", i.Src[0])
		}
		return "RET"
	case OpJmp:
		return fmt.Sprintf("JMP %s", i.Label)
	default:
		if i.Op.IsCondJump() {
			rhs := fmt.Sprintf("%d", i.Imm)
			if i.Src[1].Valid() {
				rhs = i.Src[1].String()
			}
			return fmt.Sprintf("%s %s, %s, %s", i.Op, i.Src[0], rhs, i.Label)
		}
		return i.Op.String()
	}
}

// Program is a parsed ARC program: a flat instruction stream with labels.
type Program struct {
	Instrs  []Instruction
	Structs map[string]*Struct
}

// Field is one member of a struct: an integer/float scalar, an arena handle, or
// a pointer to a struct or array.
type Field struct {
	Name   string
	Type   ValKind
	Scalar ScalarKind // Type == KindScalar
	Size   int64      // Type == KindPtr: pointed-to region size in bytes
	Struct string     // Type == KindPtr: ptr(Struct)
	Elem   ScalarKind // Type == KindPtr: ptr([Elem; Count])
	Count  int64
	Align  int64 // explicit alignment (0 = natural)
	Offset int64 // computed layout offset
}

// Width returns the field's own size in bytes within the struct.
func (f *Field) Width() int64 {
	if f.Type == KindScalar {
		return f.Scalar.Size()
	}
	return 8 // arena handle or data pointer
}

// Alignment returns the field's alignment in bytes: the explicit override if
// set, otherwise the natural alignment (the scalar width, or 8 for a pointer).
func (f *Field) Alignment() int64 {
	if f.Align > 0 {
		return f.Align
	}
	if f.Type == KindScalar {
		return f.Scalar.Size()
	}
	return 8
}

// Struct is a named layout of fields at computed byte offsets.
type Struct struct {
	Name   string
	Packed bool
	Fields []Field

	size int64
}

// Size returns the struct size in bytes (valid after Layout).
func (s *Struct) Size() int64 { return s.size }

// Layout assigns each field an offset and computes the struct size. Natural
// alignment (the default) pads each field to its own alignment; a packed struct
// has no padding. An explicit per-field align overrides both.
func (s *Struct) Layout() {
	var off, maxAlign int64 = 0, 1
	for i := range s.Fields {
		f := &s.Fields[i]
		al := int64(1)
		switch {
		case f.Align > 0:
			al = f.Align
		case !s.Packed:
			al = f.Alignment()
		}
		off = roundUp(off, al)
		f.Offset = off
		off += f.Width()
		if al > maxAlign {
			maxAlign = al
		}
	}
	if !s.Packed {
		off = roundUp(off, maxAlign)
	}
	s.size = off
}

// Offset returns the byte offset of a named field, or -1 if absent.
func (s *Struct) Offset(name string) (int64, *Field) {
	for i := range s.Fields {
		if s.Fields[i].Name == name {
			return s.Fields[i].Offset, &s.Fields[i]
		}
	}
	return -1, nil
}

func roundUp(v, a int64) int64 {
	if a <= 1 {
		return v
	}
	return (v + a - 1) / a * a
}

func formatParams(params []Param) string {
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = formatParam(p)
	}
	return strings.Join(parts, ", ")
}

func formatParam(p Param) string {
	prefix := ""
	if p.Own {
		prefix = "own "
	}
	return fmt.Sprintf("%s: %s%s", p.Reg, prefix, formatParamType(p))
}

func formatParamType(p Param) string {
	switch p.Kind {
	case KindPtr:
		switch {
		case p.Count > 0:
			return fmt.Sprintf("ptr([%s; %d])", p.Elem, p.Count)
		case p.Struct != "":
			return "ptr(" + p.Struct + ")"
		default:
			return fmt.Sprintf("ptr(%d)", p.Size)
		}
	case KindArena:
		if p.Size > 0 {
			return fmt.Sprintf("arena(%d)", p.Size)
		}
		return "arena"
	default:
		return p.Scalar.String()
	}
}

func formatRet(i Instruction) string {
	if i.RetKind == KindScalar && i.RetScalar == I64 {
		return ""
	}
	switch i.RetKind {
	case KindPtr:
		p := Param{Kind: KindPtr, Size: i.RetSize, Struct: i.RetStruct, Elem: i.RetElem, Count: i.RetCount}
		return " -> " + formatParamType(p)
	case KindArena:
		if i.RetSize > 0 {
			return fmt.Sprintf(" -> arena(%d)", i.RetSize)
		}
		return " -> arena"
	default:
		return " -> " + i.RetScalar.String()
	}
}

func formatLanes(i Instruction) string {
	parts := make([]string, len(i.Lanes))
	for j, v := range i.Lanes {
		switch i.Vec {
		case VecF32x4:
			parts[j] = fmt.Sprintf("%v", math.Float32frombits(uint32(v)))
		case VecF64x2:
			parts[j] = fmt.Sprintf("%v", math.Float64frombits(uint64(v)))
		case VecI32x4:
			parts[j] = fmt.Sprintf("%d", int32(v))
		default:
			parts[j] = fmt.Sprintf("%d", v)
		}
	}
	return strings.Join(parts, ", ")
}
