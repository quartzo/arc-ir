// Package parser reads the ARC flat-assembly text syntax.
//
// The syntax is line oriented. Each line is either a label, or an instruction.
// Comments start with ';' or '#'. Registers are r0..r15.
//
//	entry:
//	    r1 = ARENA_CREATE DRAM_LOCAL, 4096
//	    r2 = ARENA_ALLOC r1, 128
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
	prog := &ir.Program{}
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
		inst, err := parseLine(toks, lineNo)
		if err != nil {
			return nil, err
		}
		prog.Instrs = append(prog.Instrs, inst)
	}
	return prog, nil
}

func stripComment(line string) string {
	if idx := strings.IndexAny(line, ";#"); idx >= 0 {
		return line[:idx]
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
			kindTok, err := c.next()
			if err != nil {
				return ir.Instruction{}, err
			}
			kind, ok := ir.ParseValKind(kindTok)
			if !ok {
				return ir.Instruction{}, fmt.Errorf("line %d: unknown parameter type %q", lineNo, kindTok)
			}
			if own && kind != ir.KindArena {
				return ir.Instruction{}, fmt.Errorf("line %d: 'own' is only valid for arena parameters", lineNo)
			}
			param := ir.Param{Reg: reg, Kind: kind, Own: own}
			if c.peek() == "(" {
				c.pos++
				size, err := parseImm(c, lineNo)
				if err != nil {
					return ir.Instruction{}, err
				}
				if err := c.expect(")"); err != nil {
					return ir.Instruction{}, err
				}
				param.Size = size
			}
			if kind == ir.KindPtr && param.Size <= 0 {
				return ir.Instruction{}, fmt.Errorf("line %d: ptr parameter %s requires a size, e.g. ptr(64)", lineNo, reg)
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
	// Optional return type: "-> kind" or "-> ptr(size)".
	if c.peek() == "-" && c.peekAt(1) == ">" {
		c.pos += 2
		kindTok, err := c.next()
		if err != nil {
			return ir.Instruction{}, err
		}
		kind, ok := ir.ParseValKind(kindTok)
		if !ok {
			return ir.Instruction{}, fmt.Errorf("line %d: unknown return type %q", lineNo, kindTok)
		}
		inst.RetKind = kind
		if c.peek() == "(" {
			c.pos++
			size, err := parseImm(c, lineNo)
			if err != nil {
				return ir.Instruction{}, err
			}
			if err := c.expect(")"); err != nil {
				return ir.Instruction{}, err
			}
			inst.RetSize = size
		}
		if kind == ir.KindPtr && inst.RetSize <= 0 {
			return ir.Instruction{}, fmt.Errorf("line %d: ptr return type requires a size, e.g. -> ptr(64)", lineNo)
		}
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
		size, err := parseImm(c, lineNo)
		if err != nil {
			return inst, err
		}
		inst.Size = size

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
