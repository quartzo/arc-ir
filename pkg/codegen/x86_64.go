// Package codegen lowers verified ARC assembly to x86_64 GNU assembly.
//
// ARC's flat register file maps almost directly onto the machine. The backend
// assigns each ARC register used anywhere in the module to a physical register
// (preferring callee-saved ones) and spills the remainder to stack slots. The
// mapping is global, so argument registers (r0..r4) hold the same physical
// register in caller and callee.
//
// Each ARC function becomes a separate assembly function with its own frame.
// Around a call every ARC register is saved to its stack slot and restored
// afterwards, so a call is transparent to the caller except for the return
// value in r0.
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

// physical registers handed out to ARC registers, callee-saved first. %rsp is
// reserved; %r10, %r11 and %rax are reserved as scratch.
var arcPhysRegs = []string{
	"%rbx", "%r12", "%r13", "%r14", "%r15",
	"%rcx", "%rdx", "%rsi", "%rdi", "%r8", "%r9",
}

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
)

type loc struct {
	reg string // non-empty when resident in a register
	off int    // rbp-relative offset when spilled
}

type x64 struct {
	mod    *ir.Module
	physOf map[ir.Register]string // global register assignment
	symbol map[string]string      // function name -> assembly symbol

	// per-function state
	fn    *ir.Function
	used  []ir.Register
	slot  map[ir.Register]int
	frame int

	b strings.Builder
}

// EmitAsm translates a program to x86_64 GNU assembly. The program must have
// been verified first.
func EmitAsm(prog *ir.Program) (string, error) {
	m, err := ir.BuildModule(prog)
	if err != nil {
		return "", err
	}
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
	for i, r := range gp {
		if i < len(arcPhysRegs) {
			x.physOf[r] = arcPhysRegs[i]
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
	x.line(".section .note.GNU-stack,\"\",@progbits")
	return x.b.String(), nil
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
	x.line("\tmovq %%rsp, %%rbp")
	x.line("\tpushq %%rbx")
	x.line("\tpushq %%r12")
	x.line("\tpushq %%r13")
	x.line("\tpushq %%r14")
	x.line("\tpushq %%r15")
	if x.frame > 0 {
		x.line("\tsubq $%d, %%rsp", x.frame)
	}

	for _, b := range f.Blocks {
		x.line("%s:", x.blockLabel(b.Label))
		for _, inst := range b.Instrs {
			if err := x.inst(inst); err != nil {
				return err
			}
		}
	}

	x.line("\tmovq $0, %%rax")
	x.line("%s:", x.epilogueLabel())
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

	x.slot = map[ir.Register]int{}
	for i, r := range used {
		x.slot[r] = -8 * (i + 1)
	}
	x.frame = len(used) * 8
	if x.frame%16 != 8 {
		x.frame += 8
	}
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
		x.saveAll()
		x.line("\tcall %s", x.symbol[inst.Label])
		x.line("\tmovq %%rax, %s", scratch2)
		x.restoreAll()
		x.store(scratch2, 0) // return value in r0

	case ir.OpLoad:
		addr := x.memOperand(inst.Mem)
		if x.physOf[inst.Dst] != "" {
			x.line("\tmovq %s, %s", addr, x.physOf[inst.Dst])
		} else {
			x.line("\tmovq %s, %s", addr, scratch3)
			x.store(scratch3, inst.Dst)
		}

	case ir.OpStore:
		src := x.load(inst.Src[0], scratch3)
		addr := x.memOperand(inst.Mem)
		x.line("\tmovq %s, %s", src, addr)

	case ir.OpAdd, ir.OpSub, ir.OpMul:
		a := x.load(inst.Src[0], scratch1)
		x.line("\tmovq %s, %s", a, scratch3)
		rhs := x.rhsOperand(inst)
		mnem := map[ir.OpCode]string{ir.OpAdd: "addq", ir.OpSub: "subq", ir.OpMul: "imulq"}[inst.Op]
		x.line("\t%s %s, %s", mnem, rhs, scratch3)
		x.store(scratch3, inst.Dst)

	case ir.OpFConst:
		x.movImm(int64(math.Float64bits(inst.FImm)), scratch3)
		if phys := x.physOf[inst.Dst]; phys != "" {
			x.line("\tmovq %s, %s", scratch3, phys)
		} else {
			x.line("\tmovq %s, %s", scratch3, xmmScratch1)
			x.line("\tmovsd %s, %d(%%rbp)", xmmScratch1, x.slot[inst.Dst])
		}

	case ir.OpFAdd, ir.OpFSub, ir.OpFMul, ir.OpFDiv:
		a := x.loadF(inst.Src[0], xmmScratch1)
		if a != xmmScratch1 {
			x.line("\tmovsd %s, %s", a, xmmScratch1)
		}
		b := x.loadF(inst.Src[1], xmmScratch2)
		fmnem := map[ir.OpCode]string{ir.OpFAdd: "addsd", ir.OpFSub: "subsd", ir.OpFMul: "mulsd", ir.OpFDiv: "divsd"}[inst.Op]
		x.line("\t%s %s, %s", fmnem, b, xmmScratch1)
		x.store(xmmScratch1, inst.Dst)

	case ir.OpIToF:
		src := x.load(inst.Src[0], scratch1)
		if phys := x.physOf[inst.Dst]; phys != "" {
			x.line("\tcvtsi2sdq %s, %s", src, phys)
		} else {
			x.line("\tcvtsi2sdq %s, %s", src, xmmScratch1)
			x.line("\tmovsd %s, %d(%%rbp)", xmmScratch1, x.slot[inst.Dst])
		}

	case ir.OpFToI:
		src := x.loadF(inst.Src[0], xmmScratch1)
		x.line("\tcvttsd2siq %s, %s", src, scratch3)
		x.store(scratch3, inst.Dst)

	case ir.OpPrintF:
		x.saveAll()
		src := x.loadF(inst.Src[0], xmmScratch1)
		if src != "%xmm0" {
			x.line("\tmovsd %s, %%xmm0", src)
		}
		x.line("\tcall arc_print_f64")
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

// load returns the physical register holding r, spilling from the stack slot
// into scratch when r is not register-resident.
func (x *x64) load(r ir.Register, scratch string) string {
	if phys := x.physOf[r]; phys != "" {
		return phys
	}
	if r.IsVec() {
		x.line("\tmovsd %d(%%rbp), %s", x.slot[r], xmmScratch1)
		return xmmScratch1
	}
	x.line("\tmovq %d(%%rbp), %s", x.slot[r], scratch)
	return scratch
}

// loadF is load for a float operand into a chosen XMM scratch.
func (x *x64) loadF(r ir.Register, scratch string) string {
	if phys := x.physOf[r]; phys != "" {
		return phys
	}
	x.line("\tmovsd %d(%%rbp), %s", x.slot[r], scratch)
	return scratch
}

// store writes src into the home of r.
func (x *x64) store(src string, r ir.Register) {
	if phys := x.physOf[r]; phys != "" {
		if phys != src {
			if r.IsVec() {
				x.line("\tmovsd %s, %s", src, phys)
			} else {
				x.line("\tmovq %s, %s", src, phys)
			}
		}
		return
	}
	if r.IsVec() {
		x.line("\tmovsd %s, %d(%%rbp)", src, x.slot[r])
	} else {
		x.line("\tmovq %s, %d(%%rbp)", src, x.slot[r])
	}
}

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
		if phys := x.physOf[r]; phys != "" {
			if r.IsVec() {
				x.line("\tmovsd %s, %d(%%rbp)", phys, x.slot[r])
			} else {
				x.line("\tmovq %s, %d(%%rbp)", phys, x.slot[r])
			}
		}
	}
}

func (x *x64) restoreAll() {
	for _, r := range x.used {
		if phys := x.physOf[r]; phys != "" {
			if r.IsVec() {
				x.line("\tmovsd %d(%%rbp), %s", x.slot[r], phys)
			} else {
				x.line("\tmovq %d(%%rbp), %s", x.slot[r], phys)
			}
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
	}
	return "jmp"
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
