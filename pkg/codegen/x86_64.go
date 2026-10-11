// Package codegen lowers verified ARC assembly to x86_64 GNU assembly.
//
// ARC's flat register file maps onto a small set of physical registers plus a
// fixed per-thread memory area. The backend assigns each ARC register used
// anywhere in the module either a physical register or an area slot; the
// mapping is global, so argument registers (r0..r4) hold the same home in
// caller and callee. The area is addressed through a reserved base register
// (%rbp), so the same logical register is reached the same way everywhere.
//
// Scratch is volatile and gets caller-saved physical registers: a call may
// clobber them. Preserved is callee-saved and gets callee-saved physical
// registers (plus a few extra registers, whose area/register home the callee
// backs up on the stack if it writes them). Vector registers spill to the
// stack.
package codegen

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"arc-ir/pkg/ir"
	"arc-ir/pkg/runtime"
)

// physical registers handed out to each block. Scratch is volatile, so it gets
// caller-saved registers (nothing has to survive a call); preserved gets the
// callee-saved ones. Registers with no physical home live in the area.
var (
	arcScratchPhys   = []string{"%rcx", "%rdx", "%rsi", "%rdi", "%r8"}
	arcPreservedPhys = []string{"%rbx", "%r12", "%r13", "%r14", "%r15", "%r9"}
)

// vector registers handed out to ARC vector registers; %xmm14 and %xmm15 are
// reserved as scratch.
var xmmRegs = []string{
	"%xmm0", "%xmm1", "%xmm2", "%xmm3", "%xmm4", "%xmm5", "%xmm6", "%xmm7",
	"%xmm8", "%xmm9", "%xmm10", "%xmm11", "%xmm12", "%xmm13",
}

const (
	scratch1    = "%r10"
	scratch2    = "%r11"
	scratch3    = "%rax"
	xmmScratch1 = "%xmm14"
	xmmScratch2 = "%xmm15"

	// areaBase is the reserved register that holds the address of the fixed
	// register area; areaSymbol is the area itself (a static region).
	areaBase   = "%rbp"
	areaSymbol = "arc_regsv"
)

// The prologue always pushes six callee-saved registers (rbp, rbx, r12-r15):
// 48 bytes, a multiple of 16, so the frame size must be ≡ 8 (mod 16) to keep
// rsp 16-byte aligned at a call.

type x64 struct {
	mod    *ir.Module
	physOf map[ir.Register]string // global register assignment
	symbol map[string]string      // function name -> assembly symbol

	// per-function state
	fn         *ir.Function
	used       []ir.Register
	preserved  []ir.Register       // written preserved regs backed up on the stack
	slot       map[ir.Register]int // vector slot (rsp-relative)
	backup     map[ir.Register]int // preserved backup slot (rsp-relative)
	frame      int
	vecScratch int // rsp-relative offset of a 16-byte scratch for vector prints
	consts     [][16]byte

	b strings.Builder
}

// EmitAsm translates a verified module to x86_64 GNU assembly. The module must
// come from the verifier, which has lowered field accesses to plain loads and
// stores.
func EmitAsm(m *ir.Module) (string, error) {
	x := &x64{mod: m, physOf: map[ir.Register]string{}, symbol: map[string]string{}}
	x.assignGlobals()
	return x.emit()
}

// RuntimeSource returns the C support runtime emitted alongside generated asm.
func RuntimeSource() string { return runtime.Source }

func (x *x64) assignGlobals() {
	var gp, vec []ir.Register
	seen := map[ir.Register]bool{}
	for _, f := range x.mod.Funcs {
		funcRegs(f, func(r ir.Register) {
			if r.Valid() && !seen[r] {
				seen[r] = true
				if r.IsVec() {
					vec = append(vec, r)
				} else {
					gp = append(gp, r)
				}
			}
		})
	}
	sort.Slice(gp, func(i, j int) bool { return gp[i] < gp[j] })
	sort.Slice(vec, func(i, j int) bool { return vec[i] < vec[j] })
	// Each block gets its physical registers first (in order); the rest live in
	// the register area (addressed through areaBase).
	var scratch, preserved []ir.Register
	for _, r := range gp {
		if r.IsScratch() {
			scratch = append(scratch, r)
		} else {
			preserved = append(preserved, r)
		}
	}
	for i, r := range scratch {
		if i < len(arcScratchPhys) {
			x.physOf[r] = arcScratchPhys[i]
		}
	}
	for i, r := range preserved {
		if i < len(arcPreservedPhys) {
			x.physOf[r] = arcPreservedPhys[i]
		}
	}
	for i, r := range vec {
		if i < len(xmmRegs) {
			x.physOf[r] = xmmRegs[i]
		}
	}

	for _, f := range x.mod.Funcs {
		if f.IsEntry {
			x.symbol[f.Name] = "main"
		} else {
			x.symbol[f.Name] = ".Lfunc_" + sanitize(f.Name)
		}
	}
}

// funcRegs visits every register referenced by a function.
func funcRegs(f *ir.Function, fn func(ir.Register)) {
	for _, b := range f.Blocks {
		for _, inst := range b.Instrs {
			if inst.Op == ir.OpLabel {
				continue
			}
			if inst.Op.HasDest() {
				fn(inst.Dst)
			}
			fn(inst.Src[0])
			fn(inst.Src[1])
			switch inst.Op {
			case ir.OpLoad, ir.OpStore:
				fn(inst.Mem.Base)
				fn(inst.Mem.Index)
			case ir.OpCall:
				fn(0) // return value register
			}
		}
	}
}

func (x *x64) emit() (string, error) {
	x.line(".text")
	for _, f := range x.mod.Funcs {
		if err := x.emitFunc(f); err != nil {
			return "", err
		}
	}
	x.emitRodata()
	x.emitArea()
	x.line(".section .note.GNU-stack,\"\",@progbits")
	return x.b.String(), nil
}

// emitArea declares the fixed register area: one 8-byte slot per general-purpose
// register, used as the home of any register that has no physical one.
func (x *x64) emitArea() {
	x.line(".bss")
	x.line(".align 16")
	x.line("%s:", areaSymbol)
	x.line("\t.zero %d", ir.NumGP*8)
}

func (x *x64) emitFunc(f *ir.Function) error {
	x.fn = f
	x.prepare(f)

	if f.IsEntry {
		x.line(".globl main")
		x.line("main:")
	} else {
		x.line("%s:", x.symbol[f.Name])
	}
	x.line("\tpushq %%rbp")
	x.line("\tleaq %s(%%rip), %%rbp", areaSymbol)
	x.line("\tpushq %%rbx")
	x.line("\tpushq %%r12")
	x.line("\tpushq %%r13")
	x.line("\tpushq %%r14")
	x.line("\tpushq %%r15")
	if x.frame > 0 {
		x.line("\tsubq $%d, %%rsp", x.frame)
	}
	for _, r := range x.preserved {
		x.line("\tmovq %s, %s", x.home(r), scratch1)
		x.line("\tmovq %s, %d(%%rsp)", scratch1, x.backup[r])
	}

	for _, b := range f.Blocks {
		x.line("%s:", x.blockLabel(b.Label))
		for _, inst := range b.Instrs {
			if err := x.inst(inst); err != nil {
				return err
			}
		}
	}

	// Every block ends in a terminator (the verifier rejects falling off the
	// end), so this is only a defensive default for the implicit epilogue.
	x.line("\tmovq $0, %%rax")
	x.line("%s:", x.epilogueLabel())
	for i := len(x.preserved) - 1; i >= 0; i-- {
		r := x.preserved[i]
		x.line("\tmovq %d(%%rsp), %s", x.backup[r], scratch1)
		x.line("\tmovq %s, %s", scratch1, x.home(r))
	}
	x.line("\taddq $%d, %%rsp", x.frame)
	x.line("\tpopq %%r15")
	x.line("\tpopq %%r14")
	x.line("\tpopq %%r13")
	x.line("\tpopq %%r12")
	x.line("\tpopq %%rbx")
	x.line("\tpopq %%rbp")
	x.line("\tret")
	return nil
}

// prepare computes the per-function register list, stack slots and frame size.
func (x *x64) prepare(f *ir.Function) {
	seen := map[ir.Register]bool{}
	var used []ir.Register
	funcRegs(f, func(r ir.Register) {
		if r.Valid() && !seen[r] {
			seen[r] = true
			used = append(used, r)
		}
	})
	sort.Slice(used, func(i, j int) bool { return used[i] < used[j] })
	x.used = used

	// Preserved registers this function writes must survive a call. One whose
	// home is a pushed callee-saved register is already covered; any other home
	// (a caller-saved register or the area) is backed up on the stack here.
	// Scratch is volatile and is never saved.
	wrote := map[ir.Register]bool{}
	for _, b := range f.Blocks {
		for _, inst := range b.Instrs {
			if inst.Op.HasDest() && inst.Dst.Valid() {
				wrote[inst.Dst] = true
			}
		}
	}

	// Stack slots live at non-negative offsets from rsp: a 16-byte scratch for
	// vector prints, then a 16-byte slot per used vector register, then an
	// 8-byte backup per preserved register above.
	x.slot = map[ir.Register]int{}
	x.backup = map[ir.Register]int{}
	off := 0
	x.vecScratch = off
	off += 16
	for _, r := range used {
		if r.IsVec() {
			x.slot[r] = off
			off += 16
		}
	}
	x.preserved = nil
	for _, r := range used {
		if r.IsPreserved() && wrote[r] && !x.pushed(r) {
			x.backup[r] = off
			off += 8
			x.preserved = append(x.preserved, r)
		}
	}
	x.frame = off
	if x.frame%16 != 8 {
		x.frame += 8
	}
}

// pushed reports whether r's physical home is one of the registers the prologue
// pushes, so that a call already preserves it.
func (x *x64) pushed(r ir.Register) bool {
	switch x.physOf[r] {
	case "%rbx", "%r12", "%r13", "%r14", "%r15":
		return true
	}
	return false
}

func (x *x64) inst(inst ir.Instruction) error {
	switch inst.Op {
	case ir.OpMov:
		if inst.Src[0].Valid() {
			src := x.load(inst.Src[0], scratch1)
			x.store(src, inst.Dst)
		} else {
			x.movImmToDst(inst.Imm, inst.Dst)
		}

	case ir.OpArenaCreate:
		x.saveAll()
		x.movImm(int64(inst.Tier), "%rdi")
		x.movImm(inst.Size, "%rsi")
		fn := "arc_arena_create"
		if inst.Growable {
			fn = "arc_arena_create_growable"
		}
		x.line("\tcall %s", fn)
		x.line("\tmovq %%rax, %s", scratch2)
		x.restoreAll()
		x.store(scratch2, inst.Dst)

	case ir.OpArenaAlloc:
		x.saveAll()
		x.line("\tmovq %s, %%rdi", x.load(inst.Src[0], scratch1))
		x.movImm(inst.Size, "%rsi")
		x.line("\tcall arc_arena_alloc")
		x.line("\tmovq %%rax, %s", scratch2)
		x.restoreAll()
		x.store(scratch2, inst.Dst)

	case ir.OpArenaDestroy:
		x.saveAll()
		x.line("\tmovq %s, %%rdi", x.load(inst.Src[0], scratch1))
		x.line("\tcall arc_arena_destroy")
		x.restoreAll()

	case ir.OpPrint:
		x.saveAll()
		x.line("\tmovq %s, %%rdi", x.load(inst.Src[0], scratch1))
		x.line("\tcall arc_print")
		x.restoreAll()

	case ir.OpCall:
		x.line("\tcall %s", x.symbol[inst.Label])
		x.line("\tmovq %%rax, %s", scratch2)
		x.store(scratch2, 0) // return value in r0

	case ir.OpLoad:
		addr := x.memOperand(inst.Mem)
		if inst.Dst.IsVec() {
			mnem := "movsd"
			if inst.Width == 4 {
				mnem = "movss"
			}
			if phys := x.physOf[inst.Dst]; phys != "" {
				x.line("\t%s %s, %s", mnem, addr, phys)
			} else {
				x.line("\t%s %s, %s", mnem, addr, xmmScratch1)
				x.store(xmmScratch1, inst.Dst)
			}
			return nil
		}
		mnem := intLoadMnem(inst.Width, inst.Signed)
		if phys := x.physOf[inst.Dst]; phys != "" {
			x.line("\t%s %s, %s", mnem, addr, loadDst(phys, inst.Width, inst.Signed))
		} else {
			x.line("\t%s %s, %s", mnem, addr, loadDst(scratch3, inst.Width, inst.Signed))
			x.store(scratch3, inst.Dst)
		}

	case ir.OpStore:
		addr := x.memOperand(inst.Mem)
		if inst.Src[0].IsVec() {
			mnem := "movsd"
			if inst.Width == 4 {
				mnem = "movss"
			}
			src := x.loadF(inst.Src[0], xmmScratch1)
			x.line("\t%s %s, %s", mnem, src, addr)
			return nil
		}
		src := x.load(inst.Src[0], scratch3)
		if inst.Width < 8 {
			src = subReg(src, inst.Width)
		}
		x.line("\t%s %s, %s", intStoreMnem(inst.Width), src, addr)

	case ir.OpVariant:
		base := x.load(inst.Src[0], scratch1)
		x.line("\tmovb $%d, 0(%s)", inst.Variant, base)

	case ir.OpJvariant:
		base := x.load(inst.Src[0], scratch1)
		x.line("\tcmpb $%d, 0(%s)", inst.Variant, base)
		x.line("\tje %s", x.blockLabel(inst.Label))

	case ir.OpAdd, ir.OpSub, ir.OpMul:
		a := x.load(inst.Src[0], scratch1)
		x.line("\tmovq %s, %s", a, scratch3)
		rhs := x.rhsOperand(inst)
		mnem := map[ir.OpCode]string{ir.OpAdd: "addq", ir.OpSub: "subq", ir.OpMul: "imulq"}[inst.Op]
		x.line("\t%s %s, %s", mnem, rhs, scratch3)
		x.store(scratch3, inst.Dst)

	case ir.OpFConst:
		if inst.F32 {
			x.movImm(int64(math.Float32bits(float32(inst.FImm))), scratch3)
			if phys := x.physOf[inst.Dst]; phys != "" {
				x.line("\tmovd %%eax, %s", phys)
			} else {
				x.line("\tmovd %%eax, %s", xmmScratch1)
				x.store(xmmScratch1, inst.Dst)
			}
		} else {
			x.movImm(int64(math.Float64bits(inst.FImm)), scratch3)
			if phys := x.physOf[inst.Dst]; phys != "" {
				x.line("\tmovq %s, %s", scratch3, phys)
			} else {
				x.line("\tmovq %s, %s", scratch3, xmmScratch1)
				x.store(xmmScratch1, inst.Dst)
			}
		}

	case ir.OpFAdd, ir.OpFSub, ir.OpFMul, ir.OpFDiv:
		suf := "sd"
		if inst.Width == 4 {
			suf = "ss"
		}
		a := x.loadF(inst.Src[0], xmmScratch1)
		if a != xmmScratch1 {
			x.line("\tmov%s %s, %s", suf, a, xmmScratch1)
		}
		b := x.loadF(inst.Src[1], xmmScratch2)
		op := map[ir.OpCode]string{ir.OpFAdd: "add", ir.OpFSub: "sub", ir.OpFMul: "mul", ir.OpFDiv: "div"}[inst.Op]
		x.line("\t%s%s %s, %s", op, suf, b, xmmScratch1)
		x.store(xmmScratch1, inst.Dst)

	case ir.OpIToF:
		src := x.load(inst.Src[0], scratch1)
		if phys := x.physOf[inst.Dst]; phys != "" {
			x.line("\tcvtsi2sdq %s, %s", src, phys)
		} else {
			x.line("\tcvtsi2sdq %s, %s", src, xmmScratch1)
			x.store(xmmScratch1, inst.Dst)
		}

	case ir.OpFToI:
		src := x.loadF(inst.Src[0], xmmScratch1)
		if inst.Width == 4 {
			x.line("\tcvttss2siq %s, %s", src, scratch3)
		} else {
			x.line("\tcvttsd2siq %s, %s", src, scratch3)
		}
		x.store(scratch3, inst.Dst)

	case ir.OpPrintF:
		x.saveAll()
		src := x.loadF(inst.Src[0], xmmScratch1)
		if inst.Width == 4 {
			x.line("\tcvtss2sd %s, %%xmm0", src)
		} else if src != "%xmm0" {
			x.line("\tmovsd %s, %%xmm0", src)
		}
		x.line("\tcall arc_print_f64")
		x.restoreAll()

	case ir.OpIToF32:
		src := x.load(inst.Src[0], scratch1)
		if phys := x.physOf[inst.Dst]; phys != "" {
			x.line("\tcvtsi2ssq %s, %s", src, phys)
		} else {
			x.line("\tcvtsi2ssq %s, %s", src, xmmScratch1)
			x.store(xmmScratch1, inst.Dst)
		}

	case ir.OpVConst:
		lbl := x.vecConstLabel(inst.Vec, inst.Lanes)
		if phys := x.physOf[inst.Dst]; phys != "" {
			x.line("\tmovdqu %s(%%rip), %s", lbl, phys)
		} else {
			x.line("\tmovdqu %s(%%rip), %s", lbl, xmmScratch1)
			x.store(xmmScratch1, inst.Dst)
		}

	case ir.OpVAdd, ir.OpVSub, ir.OpVMul, ir.OpVDiv:
		a := x.loadF(inst.Src[0], xmmScratch1)
		if a != xmmScratch1 {
			x.line("\tmovdqu %s, %s", a, xmmScratch1)
		}
		b := x.loadF(inst.Src[1], xmmScratch2)
		x.line("\t%s %s, %s", vecMnem(inst.Op, inst.Vec), b, xmmScratch1)
		x.store(xmmScratch1, inst.Dst)

	case ir.OpPrintV:
		x.saveAll()
		src := x.loadF(inst.Src[0], xmmScratch1)
		x.line("\tmovdqu %s, %d(%%rsp)", src, x.vecScratch)
		n, step := 4, 4
		if inst.Vec == ir.VecF64x2 || inst.Vec == ir.VecI64x2 {
			n, step = 2, 8
		}
		for i := 0; i < n; i++ {
			off := x.vecScratch + i*step
			switch inst.Vec {
			case ir.VecF32x4:
				x.line("\tmovss %d(%%rsp), %%xmm0", off)
				x.line("\tcvtss2sd %%xmm0, %%xmm0")
				x.line("\tcall arc_print_f64")
			case ir.VecF64x2:
				x.line("\tmovsd %d(%%rsp), %%xmm0", off)
				x.line("\tcall arc_print_f64")
			case ir.VecI32x4:
				x.line("\tmovslq %d(%%rsp), %%rdi", off)
				x.line("\tcall arc_print")
			default: // i64x2
				x.line("\tmovq %d(%%rsp), %%rdi", off)
				x.line("\tcall arc_print")
			}
		}
		x.restoreAll()

	case ir.OpJmp:
		x.line("\tjmp %s", x.blockLabel(inst.Label))

	case ir.OpRet:
		if inst.Src[0].Valid() {
			x.line("\tmovq %s, %%rax", x.load(inst.Src[0], scratch3))
		} else {
			x.line("\tmovq $0, %%rax")
		}
		x.line("\tjmp %s", x.epilogueLabel())

	default:
		if inst.Op.IsCondJump() {
			a := x.load(inst.Src[0], scratch1)
			rhs := x.rhsOperand(inst)
			x.line("\tcmpq %s, %s", rhs, a)
			x.line("\t%s %s", jccMnemonic(inst.Op), x.blockLabel(inst.Label))
			return nil
		}
		return fmt.Errorf("x86_64 codegen: unsupported instruction %s", inst.Op)
	}
	return nil
}

func (x *x64) memOperand(m ir.Mem) string {
	base := x.load(m.Base, scratch1)
	if m.Index.Valid() {
		idx := x.load(m.Index, scratch2)
		return fmt.Sprintf("%d(%s,%s,%d)", m.Offset, base, idx, m.Scale)
	}
	return fmt.Sprintf("%d(%s)", m.Offset, base)
}

func (x *x64) rhsOperand(inst ir.Instruction) string {
	r, imm, isReg := inst.RHS()
	if isReg {
		return x.load(r, scratch2)
	}
	return fmt.Sprintf("$%d", imm)
}

// load returns the register or memory operand holding r, spilling into scratch
// when r is not register-resident.
func (x *x64) load(r ir.Register, scratch string) string {
	if phys := x.physOf[r]; phys != "" {
		return phys
	}
	if r.IsVec() {
		x.line("\tmovdqu %d(%%rsp), %s", x.slot[r], xmmScratch1)
		return xmmScratch1
	}
	x.line("\tmovq %d(%s), %s", areaOff(r), areaBase, scratch)
	return scratch
}

// loadF is load for a float operand into a chosen XMM scratch.
func (x *x64) loadF(r ir.Register, scratch string) string {
	if phys := x.physOf[r]; phys != "" {
		return phys
	}
	x.line("\tmovdqu %d(%%rsp), %s", x.slot[r], scratch)
	return scratch
}

// store writes src into the home of r.
func (x *x64) store(src string, r ir.Register) {
	if phys := x.physOf[r]; phys != "" {
		if phys != src {
			if r.IsVec() {
				x.line("\tmovdqu %s, %s", src, phys)
			} else {
				x.line("\tmovq %s, %s", src, phys)
			}
		}
		return
	}
	if r.IsVec() {
		x.line("\tmovdqu %s, %d(%%rsp)", src, x.slot[r])
	} else {
		x.line("\tmovq %s, %d(%s)", src, areaOff(r), areaBase)
	}
}

// home names the home of r: a physical register, or its slot in the register
// area.
func (x *x64) home(r ir.Register) string {
	if phys := x.physOf[r]; phys != "" {
		return phys
	}
	return fmt.Sprintf("%d(%s)", areaOff(r), areaBase)
}

// areaOff returns the offset of general-purpose register r in the register area.
func areaOff(r ir.Register) int { return int(r) * 8 }

func (x *x64) movImm(imm int64, physReg string) {
	if imm >= math.MinInt32 && imm <= math.MaxInt32 {
		x.line("\tmovq $%d, %s", imm, physReg)
	} else {
		x.line("\tmovabsq $%d, %s", imm, physReg)
	}
}

func (x *x64) movImmToDst(imm int64, dst ir.Register) {
	if phys := x.physOf[dst]; phys != "" {
		x.movImm(imm, phys)
		return
	}
	x.movImm(imm, scratch3)
	x.store(scratch3, dst)
}

func (x *x64) saveAll() {
	for _, r := range x.used {
		phys := x.physOf[r]
		if phys == "" {
			continue
		}
		if r.IsVec() {
			x.line("\tmovdqu %s, %d(%%rsp)", phys, x.slot[r])
		} else {
			x.line("\tmovq %s, %d(%s)", phys, areaOff(r), areaBase)
		}
	}
}

func (x *x64) restoreAll() {
	for _, r := range x.used {
		phys := x.physOf[r]
		if phys == "" {
			continue
		}
		if r.IsVec() {
			x.line("\tmovdqu %d(%%rsp), %s", x.slot[r], phys)
		} else {
			x.line("\tmovq %d(%s), %s", areaOff(r), areaBase, phys)
		}
	}
}

func (x *x64) line(format string, args ...any) {
	fmt.Fprintf(&x.b, format, args...)
	x.b.WriteByte('\n')
}

func (x *x64) blockLabel(label string) string {
	return ".L_" + sanitize(x.fn.Name) + "_" + sanitize(label)
}

func (x *x64) epilogueLabel() string {
	return ".L_epilogue_" + sanitize(x.fn.Name)
}

func jccMnemonic(op ir.OpCode) string {
	switch op {
	case ir.OpJeq:
		return "je"
	case ir.OpJne:
		return "jne"
	case ir.OpJlt:
		return "jl"
	case ir.OpJle:
		return "jle"
	case ir.OpJgt:
		return "jg"
	case ir.OpJge:
		return "jge"
	case ir.OpJltu:
		return "jb"
	case ir.OpJleu:
		return "jbe"
	case ir.OpJgtu:
		return "ja"
	case ir.OpJgeu:
		return "jae"
	}
	return "jmp"
}

// intLoadMnem returns the load mnemonic that reads w bytes and extends the
// result to 64 bits.
func intLoadMnem(w int64, signed bool) string {
	switch w {
	case 1:
		if signed {
			return "movsbq"
		}
		return "movzbq"
	case 2:
		if signed {
			return "movswq"
		}
		return "movzwq"
	case 4:
		if signed {
			return "movslq"
		}
		return "movl" // a 32-bit load zero-extends into the 64-bit register
	}
	return "movq"
}

// loadDst returns the destination register name for a load. A 4-byte unsigned
// load targets the 32-bit sub-register (zero-extending); every other extending
// load targets a 64-bit register.
func loadDst(reg string, w int64, signed bool) string {
	if w == 4 && !signed {
		return subReg(reg, 4)
	}
	return reg
}

// intStoreMnem returns the store mnemonic for a w-byte store.
func intStoreMnem(w int64) string {
	switch w {
	case 1:
		return "movb"
	case 2:
		return "movw"
	case 4:
		return "movl"
	}
	return "movq"
}

var subRegNames = map[string][3]string{
	"%rax": {"%al", "%ax", "%eax"},
	"%rbx": {"%bl", "%bx", "%ebx"},
	"%rcx": {"%cl", "%cx", "%ecx"},
	"%rdx": {"%dl", "%dx", "%edx"},
	"%rsi": {"%sil", "%si", "%esi"},
	"%rdi": {"%dil", "%di", "%edi"},
	"%r8":  {"%r8b", "%r8w", "%r8d"},
	"%r9":  {"%r9b", "%r9w", "%r9d"},
	"%r10": {"%r10b", "%r10w", "%r10d"},
	"%r11": {"%r11b", "%r11w", "%r11d"},
	"%r12": {"%r12b", "%r12w", "%r12d"},
	"%r13": {"%r13b", "%r13w", "%r13d"},
	"%r14": {"%r14b", "%r14w", "%r14d"},
	"%r15": {"%r15b", "%r15w", "%r15d"},
}

// subReg maps a 64-bit register name to its w-byte sub-register (w in 1, 2, 4).
func subReg(reg string, w int64) string {
	names, ok := subRegNames[reg]
	if !ok {
		return reg
	}
	switch w {
	case 1:
		return names[0]
	case 2:
		return names[1]
	case 4:
		return names[2]
	}
	return reg
}

func sanitize(label string) string {
	var b strings.Builder
	for _, ch := range label {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_' {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func vecMnem(op ir.OpCode, k ir.VecKind) string {
	switch k {
	case ir.VecI32x4:
		switch op {
		case ir.OpVSub:
			return "psubd"
		case ir.OpVMul:
			return "pmulld"
		default:
			return "paddd"
		}
	case ir.VecI64x2:
		if op == ir.OpVSub {
			return "psubq"
		}
		return "paddq"
	default:
		suf := "ps"
		if k == ir.VecF64x2 {
			suf = "pd"
		}
		switch op {
		case ir.OpVSub:
			return "sub" + suf
		case ir.OpVMul:
			return "mul" + suf
		case ir.OpVDiv:
			return "div" + suf
		default:
			return "add" + suf
		}
	}
}

func (x *x64) vecConstLabel(k ir.VecKind, lanes []int64) string {
	var c [16]byte
	if k == ir.VecF64x2 || k == ir.VecI64x2 {
		for i := 0; i < 2 && i < len(lanes); i++ {
			v := uint64(lanes[i])
			for j := 0; j < 8; j++ {
				c[i*8+j] = byte(v >> (8 * j))
			}
		}
	} else {
		for i := 0; i < 4 && i < len(lanes); i++ {
			v := uint32(lanes[i])
			for j := 0; j < 4; j++ {
				c[i*4+j] = byte(v >> (8 * j))
			}
		}
	}
	x.consts = append(x.consts, c)
	return fmt.Sprintf(".LCV%d", len(x.consts)-1)
}

func (x *x64) emitRodata() {
	if len(x.consts) == 0 {
		return
	}
	x.line(".section .rodata")
	for i, c := range x.consts {
		x.line(".LCV%d:", i)
		x.line("\t.byte %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d",
			c[0], c[1], c[2], c[3], c[4], c[5], c[6], c[7],
			c[8], c[9], c[10], c[11], c[12], c[13], c[14], c[15])
	}
}
