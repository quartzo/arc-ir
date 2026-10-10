// Package ir defines the ARC flat-register assembly IR.
//
// Unlike an SSA IR, ARC models a real machine: a fixed file of 16 registers
// (r0..r15) that instructions read and write in place, plus direct jumps
// between labeled blocks. There are no virtual registers, no phi nodes, and no
// basic-block value merging — the control flow is exactly the control flow of
// the emitted machine code.
//
// The verifier (pkg/verifier) is therefore an eBPF-style abstract interpreter:
// it walks the instruction stream, tracks the type, arena binding and value
// range of every register at every program point, and merges states at jump
// targets.
package ir

import (
	"fmt"
	"strings"
)

// Register identifies one of the 16 flat machine registers.
type Register int

// NumRegs is the size of the register file.
const NumRegs = 16

// NoReg is the sentinel for "no register" (e.g. an absent index or operand).
const NoReg Register = NumRegs

// AccessSize is the width, in bytes, of a scalar load/store.
const AccessSize int64 = 8

// Valid reports whether r names a real register.
func (r Register) Valid() bool { return r >= 0 && r < NumRegs }

// String renders a register as "rN".
func (r Register) String() string {
	if !r.Valid() {
		return "<none>"
	}
	return fmt.Sprintf("r%d", int(r))
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
	OpPrint        // PRINT rsrc
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
	OpPrint:        "PRINT",
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
	case OpMov, OpArenaCreate, OpArenaAlloc, OpLoad, OpAdd, OpSub, OpMul:
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
	Op    OpCode
	Dst   Register
	Src   [2]Register
	Imm   int64
	Mem   Mem
	Label string
	Tier  TopologyTier
	Size  int64

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
