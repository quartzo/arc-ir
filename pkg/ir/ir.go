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
	// KindScalar is a 64-bit integer parameter.
	KindScalar ValKind = iota
	// KindArena is an arena-handle parameter.
	KindArena
	// KindPtr is a data-pointer parameter.
	KindPtr
)

var kindNames = map[ValKind]string{
	KindScalar: "scalar",
	KindArena:  "arena",
	KindPtr:    "ptr",
}

// String returns the textual name of the parameter type.
func (k ValKind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return "unknown"
}

// ParseValKind resolves a textual parameter type.
func ParseValKind(s string) (ValKind, bool) {
	for k, n := range kindNames {
		if n == s {
			return k, true
		}
	}
	return KindScalar, false
}

// VecKind is the element type of a SIMD vector.
type VecKind int

const (
	// VecF32x4 is four 32-bit floats.
	VecF32x4 VecKind = iota
	// VecI32x4 is four 32-bit integers.
	VecI32x4
)

var vecKindNames = map[VecKind]string{
	VecF32x4: "f32x4",
	VecI32x4: "i32x4",
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

// Param is a declared function parameter: a register and its type. For
// KindPtr parameters, Size is the size of the allocation the pointer refers
// to, which the callee's bounds checks rely on. Own marks an arena parameter
// whose ownership is transferred to the callee; otherwise it is borrowed.
type Param struct {
	Reg  Register
	Kind ValKind
	Size int64
	Own  bool
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
	OpAdd          // rdst = ADD ra, (rb | imm)
	OpSub          // rdst = SUB ra, (rb | imm)
	OpMul          // rdst = MUL ra, (rb | imm)
	OpFConst       // vdst = <float literal>       (f64, or f32 when F32 is set)
	OpFAdd         // vdst = FADD vsrc, vsrc       (f64)
	OpFSub         // vdst = FSUB vsrc, vsrc       (f64)
	OpFMul         // vdst = FMUL vsrc, vsrc       (f64)
	OpFDiv         // vdst = FDIV vsrc, vsrc       (f64)
	OpF32Add       // vdst = FADD32 vsrc, vsrc     (f32)
	OpF32Sub       // vdst = FSUB32 vsrc, vsrc     (f32)
	OpF32Mul       // vdst = FMUL32 vsrc, vsrc     (f32)
	OpF32Div       // vdst = FDIV32 vsrc, vsrc     (f32)
	OpIToF         // vdst = ITOF rsrc   (integer to f64)
	OpIToF32       // vdst = ITOF32 rsrc (integer to f32)
	OpFToI         // rdst = FTOI vsrc   (f64 to integer)
	OpF32ToI       // rdst = F32TOI vsrc (f32 to integer)
	OpPrint        // PRINT rsrc
	OpPrintF       // PRINTF vsrc        (f64)
	OpPrintF32     // PRINTF32 vsrc      (f32)
	OpVConst       // vdst = f32x4(...) | i32x4(...)
	OpVAdd         // vdst = VADD.<kind> vsrc, vsrc
	OpVSub         // vdst = VSUB.<kind> vsrc, vsrc
	OpVMul         // vdst = VMUL.<kind> vsrc, vsrc
	OpVDiv         // vdst = VDIV.<kind> vsrc, vsrc
	OpPrintV       // PRINTV vsrc
	OpRet          // RET rsrc
	OpCall         // CALL name

	OpJmp // JMP label
	OpJeq // JEQ ra, (rb | imm), label
	OpJne // JNE ra, (rb | imm), label
	OpJlt // JLT ra, (rb | imm), label
	OpJle // JLE ra, (rb | imm), label
	OpJgt // JGT ra, (rb | imm), label
	OpJge // JGE ra, (rb | imm), label
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
	OpAdd:          "ADD",
	OpSub:          "SUB",
	OpMul:          "MUL",
	OpFConst:       "FCONST",
	OpFAdd:         "FADD",
	OpFSub:         "FSUB",
	OpFMul:         "FMUL",
	OpFDiv:         "FDIV",
	OpF32Add:       "FADD32",
	OpF32Sub:       "FSUB32",
	OpF32Mul:       "FMUL32",
	OpF32Div:       "FDIV32",
	OpIToF:         "ITOF",
	OpIToF32:       "ITOF32",
	OpFToI:         "FTOI",
	OpF32ToI:       "F32TOI",
	OpPrint:        "PRINT",
	OpPrintF:       "PRINTF",
	OpPrintF32:     "PRINTF32",
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
	case OpJeq, OpJne, OpJlt, OpJle, OpJgt, OpJge:
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
	case OpMov, OpArenaCreate, OpArenaAlloc, OpLoad, OpAdd, OpSub, OpMul,
		OpFConst, OpFAdd, OpFSub, OpFMul, OpFDiv,
		OpF32Add, OpF32Sub, OpF32Mul, OpF32Div,
		OpIToF, OpIToF32, OpFToI, OpF32ToI,
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
	Growable bool // OpArenaCreate: extensible arena; Size is the chunk size

	// Params holds a function's declared parameters (OpFunc only).
	Params []Param
	// RetKind/RetSize hold a function's declared return type (OpFunc only).
	// The zero value (KindScalar, 0) means a scalar return.
	RetKind ValKind
	RetSize int64

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
		return fmt.Sprintf("func %s(%s)%s:", i.Label, formatParams(i.Params), formatRet(i.RetKind, i.RetSize))
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
		return fmt.Sprintf("%sARENA_ALLOC %s, %d", dst, i.Src[0], i.Size)
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
	case OpFAdd, OpFSub, OpFMul, OpFDiv, OpF32Add, OpF32Sub, OpF32Mul, OpF32Div:
		return fmt.Sprintf("%s%s %s, %s", dst, i.Op, i.Src[0], i.Src[1])
	case OpIToF, OpIToF32, OpFToI, OpF32ToI:
		return fmt.Sprintf("%s%s %s", dst, i.Op, i.Src[0])
	case OpPrintF, OpPrintF32:
		return fmt.Sprintf("%s %s", i.Op, i.Src[0])
	case OpVConst:
		return fmt.Sprintf("%s%s(%s)", dst, i.Vec, formatLanes(i))
	case OpVAdd, OpVSub, OpVMul, OpVDiv:
		return fmt.Sprintf("%s%s.%s %s, %s", dst, i.Op, i.Vec, i.Src[0], i.Src[1])
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
	Instrs []Instruction
}

func formatParams(params []Param) string {
	parts := make([]string, len(params))
	for i, p := range params {
		prefix := ""
		if p.Own {
			prefix = "own "
		}
		parts[i] = fmt.Sprintf("%s: %s%s", p.Reg, prefix, formatType(p.Kind, p.Size))
	}
	return strings.Join(parts, ", ")
}

func formatType(kind ValKind, size int64) string {
	if kind == KindPtr {
		return fmt.Sprintf("ptr(%d)", size)
	}
	return kind.String()
}

func formatRet(kind ValKind, size int64) string {
	if kind == KindScalar {
		return ""
	}
	return " -> " + formatType(kind, size)
}

func formatLanes(i Instruction) string {
	parts := make([]string, len(i.Lanes))
	for j, v := range i.Lanes {
		if i.Vec == VecF32x4 {
			parts[j] = fmt.Sprintf("%v", math.Float32frombits(uint32(v)))
		} else {
			parts[j] = fmt.Sprintf("%d", int32(v))
		}
	}
	return strings.Join(parts, ", ")
}
