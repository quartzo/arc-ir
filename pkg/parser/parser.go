// Package parser reads the ARC flat-assembly text syntax.
//
// The syntax is line oriented. Each line is either a label, or an instruction.
// Comments start with ';' or '#'. Registers are r0..r15.
//
//	entry:
//	    r1 = ARENA_CREATE DRAM_LOCAL, 4096
//	    r2 = ARENA_ALLOC r1, [i64; 16]
//	    r3 = 0
//	loop:
//	    JGE r3, 10, exit
//	    STORE [r2 + r3*8], r3
//	    r3 = ADD r3, 1
//	    JMP loop
//	exit:
//	    ARENA_DESTROY r1
//	    RET r3
package parser

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"text/scanner"
	"unicode"

	"arc-ir/pkg/ir"
)

// Parse reads a complete ARC program.
func Parse(src string) (*ir.Program, error) {
	prog := &ir.Program{Structs: map[string]*ir.Struct{}}
	for i, raw := range strings.Split(src, "\n") {
		lineNo := i + 1
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		toks, err := tokenize(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if toks[0] == "struct" || (toks[0] == "packed" && len(toks) > 1 && toks[1] == "struct") {
			if err := parseStruct(prog, toks, lineNo); err != nil {
				return nil, err
			}
			continue
		}
		inst, err := parseLine(toks, lineNo)
		if err != nil {
			return nil, err
		}
		prog.Instrs = append(prog.Instrs, inst)
	}

	// Lay out every struct (offsets and size), then resolve pointer targets to
	// a byte size. Layout does not depend on the pointed-to sizes (a pointer is
	// always one word), so the order is safe even for self-referential structs.
	for _, st := range prog.Structs {
		st.Layout()
	}
	for i := range prog.Instrs {
		in := &prog.Instrs[i]
		if in.Op == ir.OpArenaAlloc && in.Struct != "" {
			st, ok := prog.Structs[in.Struct]
			if !ok {
				return nil, fmt.Errorf("line %d: unknown struct %q", in.Line, in.Struct)
			}
			in.Size = st.Size()
		}
		if in.Op == ir.OpFunc {
			for j := range in.Params {
				p := &in.Params[j]
				if p.Kind == ir.KindPtr && p.Struct != "" {
					st, ok := prog.Structs[p.Struct]
					if !ok {
						return nil, fmt.Errorf("line %d: unknown struct %q", in.Line, p.Struct)
					}
					p.Size = st.Size()
				}
			}
			if in.RetKind == ir.KindPtr && in.RetStruct != "" {
				st, ok := prog.Structs[in.RetStruct]
				if !ok {
					return nil, fmt.Errorf("line %d: unknown struct %q", in.Line, in.RetStruct)
				}
				in.RetSize = st.Size()
			}
		}
	}
	// Resolve struct-pointer fields to the size of the pointed-to struct.
	for _, st := range prog.Structs {
		for i := range st.Fields {
			f := &st.Fields[i]
			if f.Type == ir.KindPtr && f.Struct != "" {
				target, ok := prog.Structs[f.Struct]
				if !ok {
					return nil, fmt.Errorf("unknown struct %q in field %s.%s", f.Struct, st.Name, f.Name)
				}
				f.Size = target.Size()
			}
		}
	}
	return prog, nil
}

// parseStruct parses "struct Name { field: type, ... }" or
// "packed struct Name { ... }". A standalone "align N" directive before a field
// forces that field's offset.
func parseStruct(prog *ir.Program, toks []string, lineNo int) error {
	c := &cursor{toks: toks, line: lineNo}
	packed := false
	if c.peek() == "packed" {
		packed = true
		c.pos++
	}
	c.pos++ // skip "struct"
	name, err := c.next()
	if err != nil {
		return err
	}
	if !isIdent(name) {
		return fmt.Errorf("line %d: expected struct name, got %q", lineNo, name)
	}
	if err := c.expect("{"); err != nil {
		return err
	}
	st := &ir.Struct{Name: name, Packed: packed}
	var pendingAlign int64
	for c.peek() != "}" {
		if c.peek() == "align" {
			c.pos++
			a, err := parseImm(c, lineNo)
			if err != nil {
				return err
			}
			if a <= 0 {
				return fmt.Errorf("line %d: align must be positive, got %d", lineNo, a)
			}
			pendingAlign = a
			if c.peek() == "," {
				c.pos++
				continue
			}
			break
		}
		fname, err := c.next()
		if err != nil {
			return err
		}
		if !isIdent(fname) {
			return fmt.Errorf("line %d: expected field name, got %q", lineNo, fname)
		}
		if err := c.expect(":"); err != nil {
			return err
		}
		spec, err := parseTypeSpec(c, lineNo)
		if err != nil {
			return err
		}
		f := ir.Field{
			Name:   fname,
			Type:   spec.kind,
			Scalar: spec.scalar,
			Size:   spec.size,
			Struct: spec.structName,
			Elem:   spec.elem,
			Count:  spec.count,
			Align:  pendingAlign,
		}
		pendingAlign = 0
		st.Fields = append(st.Fields, f)
		if c.peek() == "," {
			c.pos++
			continue
		}
		break
	}
	if err := c.expect("}"); err != nil {
		return err
	}
	if err := noTrailing(c); err != nil {
		return err
	}
	if _, dup := prog.Structs[name]; dup {
		return fmt.Errorf("line %d: duplicate struct %q", lineNo, name)
	}
	prog.Structs[name] = st
	return nil
}

// typeSpec is a parsed type: a scalar, an arena, or a pointer target.
type typeSpec struct {
	kind       ir.ValKind
	scalar     ir.ScalarKind
	structName string
	elem       ir.ScalarKind
	count      int64
	size       int64
}

// parseTypeSpec parses a type: a scalar kind (i8..u64, f32, f64, or the alias
// "scalar"), "arena" / "arena(size)", or "ptr(Struct|[Elem; Count])".
func parseTypeSpec(c *cursor, lineNo int) (typeSpec, error) {
	tok, err := c.next()
	if err != nil {
		return typeSpec{}, err
	}
	if sk, ok := ir.ParseScalarKind(tok); ok {
		return typeSpec{kind: ir.KindScalar, scalar: sk}, nil
	}
	switch tok {
	case "arena":
		spec := typeSpec{kind: ir.KindArena}
		if c.peek() == "(" {
			c.pos++
			sz, err := parseImm(c, lineNo)
			if err != nil {
				return typeSpec{}, err
			}
			if err := c.expect(")"); err != nil {
				return typeSpec{}, err
			}
			spec.size = sz
		}
		return spec, nil
	case "ptr":
		structName, elem, count, size, err := parsePtrTarget(c, lineNo)
		if err != nil {
			return typeSpec{}, err
		}
		return typeSpec{kind: ir.KindPtr, structName: structName, elem: elem, count: count, size: size}, nil
	}
	return typeSpec{}, fmt.Errorf("line %d: unknown type %q", lineNo, tok)
}

// splitField splits "rbase.field" into its register and field name.
func splitField(tok string) (string, string, bool) {
	b, f, ok := strings.Cut(tok, ".")
	if !ok || !isRegister(b) || f == "" {
		return "", "", false
	}
	return b, f, true
}

// stripComment removes a ';' or '#' comment, ignoring comment characters that
// appear inside brackets (so that array types like [i64; 8] survive).
func stripComment(line string) string {
	depth := 0
	for i, ch := range line {
		switch ch {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case ';', '#':
			if depth == 0 {
				return line[:i]
			}
		}
	}
	return line
}

func tokenize(line string) ([]string, error) {
	var s scanner.Scanner
	s.Init(strings.NewReader(line))
	s.Mode = scanner.ScanIdents | scanner.ScanInts | scanner.ScanFloats
	s.Whitespace = 1<<'\t' | 1<<'\r' | 1<<' '
	s.IsIdentRune = func(ch rune, i int) bool {
		return ch == '_' || ch == '.' || unicode.IsLetter(ch) || unicode.IsDigit(ch)
	}
	var scanErr error
	s.Error = func(_ *scanner.Scanner, msg string) { scanErr = fmt.Errorf("%s", msg) }

	var toks []string
	for tok := s.Scan(); tok != scanner.EOF; tok = s.Scan() {
		if scanErr != nil {
			return nil, scanErr
		}
		toks = append(toks, s.TokenText())
	}
	return toks, nil
}

// cursor is a small token reader.
type cursor struct {
	toks []string
	pos  int
	line int
}

func (c *cursor) more() bool { return c.pos < len(c.toks) }

func (c *cursor) peek() string {
	if c.pos < len(c.toks) {
		return c.toks[c.pos]
	}
	return ""
}

func (c *cursor) peekAt(i int) string {
	if c.pos+i < len(c.toks) {
		return c.toks[c.pos+i]
	}
	return ""
}

func (c *cursor) next() (string, error) {
	if !c.more() {
		return "", fmt.Errorf("line %d: unexpected end of line", c.line)
	}
	t := c.toks[c.pos]
	c.pos++
	return t, nil
}

func (c *cursor) expect(tok string) error {
	t, err := c.next()
	if err != nil {
		return err
	}
	if t != tok {
		return fmt.Errorf("line %d: expected %q, got %q", c.line, tok, t)
	}
	return nil
}

func parseLine(toks []string, lineNo int) (ir.Instruction, error) {
	c := &cursor{toks: toks, line: lineNo}

	// Function declaration: "func name(params):".
	if toks[0] == "func" {
		return parseFunc(toks, lineNo)
	}

	// Label: "name:".
	if len(toks) >= 2 && isIdent(toks[0]) && toks[1] == ":" {
		if len(toks) > 2 {
			return ir.Instruction{}, fmt.Errorf("line %d: trailing tokens after label %q", lineNo, toks[0])
		}
		inst := ir.Instruction{Op: ir.OpLabel, Label: toks[0], Line: lineNo}
		inst.Src = [2]ir.Register{ir.NoReg, ir.NoReg}
		inst.Mem.Base = ir.NoReg
		inst.Mem.Index = ir.NoReg
		return inst, nil
	}

	// Optional destination: "rdst =".
	var dst ir.Register = ir.NoReg
	if len(toks) >= 2 && isRegister(toks[0]) && toks[1] == "=" {
		r, err := parseRegister(toks[0], lineNo)
		if err != nil {
			return ir.Instruction{}, err
		}
		dst = r
		c.pos = 2
	}

	if !c.more() {
		return ir.Instruction{}, fmt.Errorf("line %d: missing instruction", lineNo)
	}

	mnem := c.peek()

	// A vector mnemonic may carry its element type as a suffix: "VADD.f32x4".
	base := mnem
	var vec ir.VecKind
	hasVec := false
	if b, suffix, ok := strings.Cut(mnem, "."); ok {
		if k, kok := ir.ParseVecKind(strings.ToLower(suffix)); kok {
			base, vec, hasVec = b, k, true
		}
	}

	// Not a mnemonic: this is a MOV written as "rdst = <reg|imm>".
	op, isOp := ir.OpFromName(strings.ToUpper(base))
	if !isOp {
		// Field store: "rbase.field = rvalue".
		if b, f, ok := splitField(mnem); ok && c.peekAt(1) == "=" {
			base, err := parseRegister(b, lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			c.pos += 2 // consume "rbase.field" and "="
			val, err := c.reg(lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			inst := ir.Instruction{Op: ir.OpFieldStore, Field: f, Line: lineNo}
			inst.Src = [2]ir.Register{base, val}
			inst.Mem.Base = ir.NoReg
			inst.Mem.Index = ir.NoReg
			if err := noTrailing(c); err != nil {
				return ir.Instruction{}, err
			}
			return inst, nil
		}
		if !dst.Valid() {
			return ir.Instruction{}, fmt.Errorf("line %d: unknown instruction %q", lineNo, mnem)
		}
		inst := ir.Instruction{Op: ir.OpMov, Dst: dst, Line: lineNo}
		inst.Src = [2]ir.Register{ir.NoReg, ir.NoReg}
		inst.Mem.Base = ir.NoReg
		inst.Mem.Index = ir.NoReg
		if isRegister(mnem) {
			r, err := c.reg(lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			inst.Src[0] = r
		} else if b, f, ok := splitField(mnem); ok {
			base, err := parseRegister(b, lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			c.pos++ // consume the "rbase.field" token
			inst.Op = ir.OpField
			inst.Field = f
			inst.Src[0] = base
		} else if isFloatToken(mnem) || ((mnem == "-" || mnem == "+") && isFloatToken(c.peekAt(1))) {
			if !dst.IsVec() {
				return ir.Instruction{}, fmt.Errorf("line %d: a float literal requires a vector register (v0..v15)", lineNo)
			}
			v, err := parseFloat(c, lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			inst.Op = ir.OpFConst
			inst.FImm = v
		} else if mnem == "f32" && c.peekAt(1) == "(" {
			if !dst.IsVec() {
				return ir.Instruction{}, fmt.Errorf("line %d: a float literal requires a vector register (v0..v15)", lineNo)
			}
			c.pos += 2 // consume "f32" "("
			v, err := parseFloat(c, lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			if err := c.expect(")"); err != nil {
				return ir.Instruction{}, err
			}
			inst.Op = ir.OpFConst
			inst.F32 = true
			inst.FImm = v
		} else if k, ok := ir.ParseVecKind(mnem); ok && c.peekAt(1) == "(" {
			if !dst.IsVec() {
				return ir.Instruction{}, fmt.Errorf("line %d: a vector literal requires a vector register (v0..v15)", lineNo)
			}
			c.pos += 2 // consume the kind and "("
			lanes, err := parseLanes(c, k, lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			if err := c.expect(")"); err != nil {
				return ir.Instruction{}, err
			}
			inst.Op = ir.OpVConst
			inst.Vec = k
			inst.Lanes = lanes
		} else if isIdent(mnem) {
			return ir.Instruction{}, fmt.Errorf("line %d: unknown instruction %q", lineNo, mnem)
		} else {
			v, err := parseImm(c, lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			inst.Imm = v
		}
		if err := noTrailing(c); err != nil {
			return ir.Instruction{}, err
		}
		return inst, nil
	}
	c.pos++ // consume the mnemonic

	inst, err := parseOp(c, op, dst, lineNo)
	if err != nil {
		return ir.Instruction{}, err
	}
	if hasVec {
		inst.Vec = vec
	}
	if op.HasDest() && !inst.Dst.Valid() {
		return ir.Instruction{}, fmt.Errorf("line %d: %s requires a destination register", lineNo, op)
	}
	if !op.HasDest() && inst.Dst.Valid() {
		return ir.Instruction{}, fmt.Errorf("line %d: %s does not produce a result", lineNo, op)
	}
	if err := noTrailing(c); err != nil {
		return ir.Instruction{}, err
	}
	return inst, nil
}

// parseFunc parses "func name(param: type, ...):".
func parseFunc(toks []string, lineNo int) (ir.Instruction, error) {
	c := &cursor{toks: toks, line: lineNo}
	c.pos = 1 // skip "func"

	name, err := c.next()
	if err != nil {
		return ir.Instruction{}, err
	}
	if !isIdent(name) {
		return ir.Instruction{}, fmt.Errorf("line %d: expected function name, got %q", lineNo, name)
	}
	inst := ir.Instruction{Op: ir.OpFunc, Label: name, Line: lineNo}

	if c.peek() == "(" {
		c.pos++
		for c.peek() != ")" {
			reg, err := c.reg(lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			if err := c.expect(":"); err != nil {
				return ir.Instruction{}, err
			}
			own := false
			if c.peek() == "own" {
				own = true
				c.pos++
			}
			spec, err := parseTypeSpec(c, lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			if own && spec.kind != ir.KindArena {
				return ir.Instruction{}, fmt.Errorf("line %d: 'own' is only valid for arena parameters", lineNo)
			}
			if spec.kind == ir.KindScalar && spec.scalar.Float() {
				return ir.Instruction{}, fmt.Errorf("line %d: float parameters are not supported (pass a ptr instead)", lineNo)
			}
			param := ir.Param{
				Reg:    reg,
				Kind:   spec.kind,
				Scalar: spec.scalar,
				Size:   spec.size,
				Struct: spec.structName,
				Elem:   spec.elem,
				Count:  spec.count,
				Own:    own,
			}
			inst.Params = append(inst.Params, param)
			if c.peek() == "," {
				c.pos++
			} else {
				break
			}
		}
		if err := c.expect(")"); err != nil {
			return ir.Instruction{}, err
		}
	}
	// Optional return type: "-> type".
	if c.peek() == "-" && c.peekAt(1) == ">" {
		c.pos += 2
		spec, err := parseTypeSpec(c, lineNo)
		if err != nil {
			return ir.Instruction{}, err
		}
		if spec.kind == ir.KindScalar && spec.scalar.Float() {
			return ir.Instruction{}, fmt.Errorf("line %d: float return types are not supported", lineNo)
		}
		inst.RetKind = spec.kind
		inst.RetScalar = spec.scalar
		inst.RetStruct = spec.structName
		inst.RetElem = spec.elem
		inst.RetCount = spec.count
		inst.RetSize = spec.size
	}

	if err := c.expect(":"); err != nil {
		return ir.Instruction{}, err
	}
	if err := noTrailing(c); err != nil {
		return ir.Instruction{}, err
	}
	return inst, nil
}

func parseOp(c *cursor, op ir.OpCode, dst ir.Register, lineNo int) (ir.Instruction, error) {
	inst := ir.Instruction{Op: op, Dst: dst, Line: lineNo}
	inst.Src = [2]ir.Register{ir.NoReg, ir.NoReg}
	inst.Mem.Base = ir.NoReg
	inst.Mem.Index = ir.NoReg

	switch op {
	case ir.OpMov:
		// Reached only via "rdst = MOV ..."; handled as reg/imm.
		if isRegister(c.peek()) {
			r, err := c.reg(lineNo)
			if err != nil {
				return inst, err
			}
			inst.Src[0] = r
		} else {
			v, err := parseImm(c, lineNo)
			if err != nil {
				return inst, err
			}
			inst.Imm = v
		}

	case ir.OpArenaCreate:
		tier, err := c.tier(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Tier = tier
		if err := c.expect(","); err != nil {
			return inst, err
		}
		if c.peek() == "growable" {
			c.pos++
			if err := c.expect("("); err != nil {
				return inst, err
			}
			chunk, err := parseImm(c, lineNo)
			if err != nil {
				return inst, err
			}
			if err := c.expect(")"); err != nil {
				return inst, err
			}
			inst.Growable = true
			inst.Size = chunk
		} else {
			size, err := parseImm(c, lineNo)
			if err != nil {
				return inst, err
			}
			inst.Size = size
		}

	case ir.OpArenaAlloc:
		arena, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[0] = arena
		if err := c.expect(","); err != nil {
			return inst, err
		}
		switch {
		case c.peek() == "[":
			elem, count, err := parseArray(c, lineNo)
			if err != nil {
				return inst, err
			}
			inst.Elem = elem
			inst.Count = count
			inst.Size = elem.Size() * count
		case isIdent(c.peek()):
			name, err := c.next()
			if err != nil {
				return inst, err
			}
			inst.Struct = name // Size is resolved in Parse
		default:
			return inst, fmt.Errorf("line %d: ARENA_ALLOC needs a type: [Elem; Count] or a struct", lineNo)
		}

	case ir.OpArenaDestroy, ir.OpPrint, ir.OpRet:
		if op == ir.OpRet && !c.more() {
			return inst, nil // bare RET
		}
		r, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[0] = r

	case ir.OpLoad:
		mem, err := c.mem(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Mem = mem

	case ir.OpStore:
		mem, err := c.mem(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Mem = mem
		if err := c.expect(","); err != nil {
			return inst, err
		}
		src, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[0] = src

	case ir.OpAdd, ir.OpSub, ir.OpMul:
		ra, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[0] = ra
		if err := c.expect(","); err != nil {
			return inst, err
		}
		if err := parseRHS(c, &inst, lineNo); err != nil {
			return inst, err
		}

	case ir.OpFAdd, ir.OpFSub, ir.OpFMul, ir.OpFDiv, ir.OpF32Add, ir.OpF32Sub, ir.OpF32Mul, ir.OpF32Div:
		a, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[0] = a
		if err := c.expect(","); err != nil {
			return inst, err
		}
		b, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[1] = b

	case ir.OpIToF, ir.OpIToF32, ir.OpFToI, ir.OpF32ToI, ir.OpPrintF, ir.OpPrintF32:
		r, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[0] = r

	case ir.OpVAdd, ir.OpVSub, ir.OpVMul, ir.OpVDiv:
		a, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[0] = a
		if err := c.expect(","); err != nil {
			return inst, err
		}
		b, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[1] = b

	case ir.OpPrintV:
		r, err := c.reg(lineNo)
		if err != nil {
			return inst, err
		}
		inst.Src[0] = r

	case ir.OpCall:
		name, err := c.next()
		if err != nil {
			return inst, err
		}
		inst.Label = name

	case ir.OpJmp:
		label, err := c.next()
		if err != nil {
			return inst, err
		}
		inst.Label = label

	default:
		if op.IsCondJump() {
			ra, err := c.reg(lineNo)
			if err != nil {
				return inst, err
			}
			inst.Src[0] = ra
			if err := c.expect(","); err != nil {
				return inst, err
			}
			if err := parseRHS(c, &inst, lineNo); err != nil {
				return inst, err
			}
			if err := c.expect(","); err != nil {
				return inst, err
			}
			label, err := c.next()
			if err != nil {
				return inst, err
			}
			inst.Label = label
		} else {
			return inst, fmt.Errorf("line %d: unsupported instruction %s", lineNo, op)
		}
	}
	return inst, nil
}

// parseRHS parses a register or immediate second operand into Src[1]/Imm.
func parseRHS(c *cursor, inst *ir.Instruction, lineNo int) error {
	if isRegister(c.peek()) {
		r, err := c.reg(lineNo)
		if err != nil {
			return err
		}
		inst.Src[1] = r
		return nil
	}
	v, err := parseImm(c, lineNo)
	if err != nil {
		return err
	}
	inst.Imm = v
	return nil
}

func noTrailing(c *cursor) error {
	if c.more() {
		return fmt.Errorf("line %d: unexpected token %q", c.line, c.peek())
	}
	return nil
}

func (c *cursor) reg(lineNo int) (ir.Register, error) {
	t, err := c.next()
	if err != nil {
		return ir.NoReg, err
	}
	return parseRegister(t, lineNo)
}

func (c *cursor) tier(lineNo int) (ir.TopologyTier, error) {
	t, err := c.next()
	if err != nil {
		return 0, err
	}
	if tier, ok := ir.ParseTopology(t); ok {
		return tier, nil
	}
	switch t {
	case "0":
		return ir.DRAMLocal, nil
	case "1":
		return ir.CXLPooled, nil
	case "2":
		return ir.Persistent, nil
	}
	return 0, fmt.Errorf("line %d: unknown memory tier %q", lineNo, t)
}

// mem parses "[base + index*scale + offset]".
func (c *cursor) mem(lineNo int) (ir.Mem, error) {
	var m ir.Mem
	m.Index = ir.NoReg
	m.Scale = 1

	if err := c.expect("["); err != nil {
		return m, err
	}
	base, err := c.reg(lineNo)
	if err != nil {
		return m, err
	}
	m.Base = base

	for c.peek() == "+" || c.peek() == "-" {
		sign := int64(1)
		if c.peek() == "-" {
			sign = -1
		}
		c.pos++

		tok := c.peek()
		switch {
		case isRegister(tok):
			r, err := c.reg(lineNo)
			if err != nil {
				return m, err
			}
			scale := int64(1)
			if c.peek() == "*" {
				c.pos++
				s, err := parseImm(c, lineNo)
				if err != nil {
					return m, err
				}
				scale = s
			}
			if !m.Index.Valid() {
				if sign < 0 {
					return m, fmt.Errorf("line %d: index register cannot be negated", lineNo)
				}
				m.Index = r
				m.Scale = scale
			} else {
				return m, fmt.Errorf("line %d: memory operand already has an index", lineNo)
			}
		default:
			v, err := parseImm(c, lineNo)
			if err != nil {
				return m, err
			}
			m.Offset += sign * v
		}
	}

	if err := c.expect("]"); err != nil {
		return m, err
	}
	if m.Index.Valid() {
		switch m.Scale {
		case 1, 2, 4, 8:
		default:
			return m, fmt.Errorf("line %d: scale must be 1, 2, 4 or 8, got %d", lineNo, m.Scale)
		}
	}
	return m, nil
}

// parseImm parses an optional sign followed by a decimal integer.
func parseImm(c *cursor, lineNo int) (int64, error) {
	sign := int64(1)
	if c.peek() == "-" {
		sign = -1
		c.pos++
	} else if c.peek() == "+" {
		c.pos++
	}
	t, err := c.next()
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("line %d: expected integer, got %q", lineNo, t)
	}
	return sign * v, nil
}

// parseFloat parses an optional sign followed by a float literal.
func parseFloat(c *cursor, lineNo int) (float64, error) {
	sign := 1.0
	if c.peek() == "-" {
		sign = -1
		c.pos++
	} else if c.peek() == "+" {
		c.pos++
	}
	t, err := c.next()
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, fmt.Errorf("line %d: expected float, got %q", lineNo, t)
	}
	return sign * v, nil
}

// isFloatToken reports whether a token is a float literal (not an integer).
func isFloatToken(tok string) bool {
	if tok == "" {
		return false
	}
	if _, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return false
	}
	_, err := strconv.ParseFloat(tok, 64)
	return err == nil
}

// parseLanes parses the lane list of a vector literal, broadcasting a single
// lane to all lanes.
func parseLanes(c *cursor, k ir.VecKind, lineNo int) ([]int64, error) {
	count := 4
	is64 := false
	isFloat := k == ir.VecF32x4 || k == ir.VecF64x2
	switch k {
	case ir.VecF64x2, ir.VecI64x2:
		count = 2
		is64 = true
	}
	var lanes []int64
	for {
		if isFloat {
			v, err := parseFloat(c, lineNo)
			if err != nil {
				return nil, err
			}
			if is64 {
				lanes = append(lanes, int64(math.Float64bits(v)))
			} else {
				lanes = append(lanes, int64(math.Float32bits(float32(v))))
			}
		} else {
			v, err := parseImm(c, lineNo)
			if err != nil {
				return nil, err
			}
			lanes = append(lanes, v)
		}
		if c.peek() == "," {
			c.pos++
			continue
		}
		break
	}
	if len(lanes) == 1 {
		b := lanes[0]
		for len(lanes) < count {
			lanes = append(lanes, b)
		}
	}
	if len(lanes) != count {
		return nil, fmt.Errorf("line %d: a vector literal needs 1 or %d lanes, got %d", lineNo, count, len(lanes))
	}
	return lanes, nil
}

// parseArray parses "[Elem; Count]".
func parseArray(c *cursor, lineNo int) (ir.ScalarKind, int64, error) {
	if err := c.expect("["); err != nil {
		return 0, 0, err
	}
	elemTok, err := c.next()
	if err != nil {
		return 0, 0, err
	}
	elem, ok := ir.ParseScalarKind(elemTok)
	if !ok {
		return 0, 0, fmt.Errorf("line %d: unknown element type %q", lineNo, elemTok)
	}
	if err := c.expect(";"); err != nil {
		return 0, 0, err
	}
	count, err := parseImm(c, lineNo)
	if err != nil {
		return 0, 0, err
	}
	if count <= 0 {
		return 0, 0, fmt.Errorf("line %d: array count must be positive", lineNo)
	}
	if err := c.expect("]"); err != nil {
		return 0, 0, err
	}
	return elem, count, nil
}

// parsePtrTarget parses the parenthesised target of a pointer: a struct name or
// an array type.
func parsePtrTarget(c *cursor, lineNo int) (structName string, elem ir.ScalarKind, count int64, size int64, err error) {
	if err = c.expect("("); err != nil {
		return
	}
	if c.peek() == "[" {
		elem, count, err = parseArray(c, lineNo)
		if err != nil {
			return
		}
		size = elem.Size() * count
	} else {
		var name string
		name, err = c.next()
		if err != nil {
			return
		}
		if !isIdent(name) {
			err = fmt.Errorf("line %d: ptr needs a struct or array, e.g. ptr([i64; 8])", lineNo)
			return
		}
		structName = name
	}
	err = c.expect(")")
	return
}

func parseRegister(tok string, lineNo int) (ir.Register, error) {
	if !isRegister(tok) {
		return ir.NoReg, fmt.Errorf("line %d: expected register, got %q", lineNo, tok)
	}
	n, err := strconv.Atoi(tok[1:])
	if err != nil || n < 0 || n >= ir.NumGP {
		return ir.NoReg, fmt.Errorf("line %d: register %q out of range (r0..r15 or v0..v15)", lineNo, tok)
	}
	if tok[0] == 'v' {
		return ir.Register(ir.NumGP + n), nil
	}
	return ir.Register(n), nil
}

func isRegister(tok string) bool {
	if len(tok) < 2 || (tok[0] != 'r' && tok[0] != 'v') {
		return false
	}
	for _, ch := range tok[1:] {
		if !unicode.IsDigit(ch) {
			return false
		}
	}
	return true
}

func isIdent(tok string) bool {
	if tok == "" {
		return false
	}
	for i, ch := range tok {
		if ch == '_' || ch == '.' || unicode.IsLetter(ch) {
			continue
		}
		if i > 0 && unicode.IsDigit(ch) {
			continue
		}
		return false
	}
	return true
}
