# ARC Syntax: Block Terminators (plan)

**Status:** implemented — the terminator syntax (`endfunc` / `endstruct` /
`endenum`) is in place, all examples and tests were migrated, and the docs were
updated. The decisions taken: per-kind terminators, no trailing `:` on `func`,
and fields/variants may be one per line or comma-separated.
**Scope:** the `.arc` text format only. The IR, the verifier and the x86_64
backend are not affected.
**Related:** [`assembly.md`](assembly.md), [`vision.md`](vision.md),
[`registers.md`](registers.md), [`../AGENTS.md`](../AGENTS.md).

## 1. Motivation

The text format currently mixes two block conventions:

- `func name(params):` — a colon-terminated header whose body ends implicitly
  at the next `func` or at end of file;
- `struct Name { ... }` / `enum Name { ... }` — brace-delimited, written on a
  single line.

The parser is line oriented and treats indentation as cosmetic: `tokenize`
discards all whitespace (`s.Whitespace = '\t' | '\r' | ' '`), and `Parse` walks
the source line by line. Nothing marks the end of a function body — it ends
only when the next `func` appears (`ir.splitFunctions`, `pkg/ir/cfg.go`).

Two concrete problems follow:

1. **No visible block end.** A reader cannot see where a function stops, and the
   indentation of the examples suggests a scope the parser does not enforce.
2. **An overloaded `:`.** The colon is a label terminator (`loop:`), a parameter
   separator (`r1: i64`), a field separator (`val: i64`), and a function-header
   terminator (`func f(...):`). Its meaning depends on context, so
   `func f(...):` looks like a label that opens a block.

This matters because the primary producer of `.arc` is expected to be an LLM
(see the README "Where AI Connects"), and an explicit, unambiguous block end is
easier to generate and repair than an implicit one.

## 2. Decision

Use **one** block convention for all three block kinds, assembly-flavoured: an
explicit terminator keyword, no braces, and no significant whitespace.

- `func ...` … `endfunc`
- `struct ...` … `endstruct`
- `enum ...` … `endenum`

Rationale: ARC is deliberately an IR that stays *as close to machine assembly as
possible*; the only thing it adds on top of assembly is the static meaning the
verifier needs, and that meaning is erased at run time (`vision.md`). Braces are
a C/Rust construct with no machine counterpart. An explicit terminator gives the
clarity the current syntax lacks without importing a high-level block syntax.

### Proposed form

```arc
; functions — no trailing colon, explicit end
func main
    r1 = 3
    r2 = 4
    CALL add
    PRINT r0
    RET r0
endfunc

func add(r1: i64, r2: i64)
    r0 = ADD r1, r2
    RET r0
endfunc

; structs — braces replaced by a block
struct Node
    next: ptr(Node), val: i64
endstruct

packed struct P
    a: u8, b: u32
endstruct

struct Q
    a: u8
    align 8
    b: u8
endstruct

; enums — braces replaced by a block
enum Value
    Int(n: i64)
    Pair(a: i32, b: i32)
    Nil
endenum
```

Properties:

- The trailing `:` is removed from the `func` header. The colon now appears only
  as a label terminator (`loop:`) and as a type-annotation separator
  (`r1: i64`, `val: i64`).
- Fields and enum variants may be written one per line, or comma-separated on a
  line; the terminator ends the block either way.
- `packed struct` and the standalone `align N` directive keep working inside the
  block.
- Enum variant payloads stay parenthesised (`Int(n: i64)`), so there is no
  nesting and every terminator is unambiguous.
- A program with no `func` declaration is still the entry function `main`
  (unchanged).

## 3. Grammar

Current (`assembly.md` §8):

```
program    := { function }
function   := "func" ident [ "(" params ")" ] ":" { block }
block      := { instruction }
```

Proposed:

```
program    := { top }
top        := function | structdef | enumdef
function   := "func" ident [ "(" params ")" ] [ "->" type ] { instruction } "endfunc"
structdef  := [ "packed" ] "struct" ident { field | "align" int } "endstruct"
enumdef    := "enum" ident { variant } "endenum"
field      := ident ":" type
variant    := ident [ "(" { field } ")" ]
```

## 4. Alternatives considered

| Option | Shape | Verdict |
|--------|-------|---------|
| **Keyword terminator** (chosen) | `func …` … `endfunc` | Assembly-flavoured, explicit end, one convention. |
| Braces everywhere (C/Rust) | `func f() { … }` | Rejected: contradicts "as close to assembly as possible"; imports a high-level block concept. |
| Significant indentation (Python) | `func f():` + indent | Rejected: whitespace-significant is fragile for generated code and unusual for assembly. |
| Single `end` | `func …` … `end` | Viable, but less self-documenting than `endfunc`; "which block ends?" is exactly the original complaint. |
| `end func` (Ada-style) | `func …` … `end func` | Viable; two-token terminator, no benefit over `endfunc`. |
| Status quo (implicit end) | body ends at next `func` | Rejected: this is the problem being fixed. |

## 5. Parser changes (`pkg/parser/parser.go`)

The IR does not change: `OpFunc` still marks function boundaries, and
`ir.splitFunctions` continues to split on it; structs and enums are still stored
in `prog.Structs` / `prog.Enums`. Only the line loop in `Parse` changes.

Turn `Parse`'s loop into a small block state machine:

```
state := none
for each line:
    toks := tokenize(stripComment(line))
    switch state:
    case none:
        if toks[0] == "struct" or "packed struct":  state = struct;  parseStructHeader(toks)
        else if toks[0] == "enum":                   state = enum;    parseEnumHeader(toks)
        else if toks[0] == "func":                   state = func;    emit OpFunc header
        else if toks[0] == "endfunc" / "endstruct" / "endenum": error "no open block"
        else:                                        instruction (leading/main or current func body)
    case func:
        if toks[0] == "endfunc": state = none
        else: parseLine(toks) and append
    case struct:
        if toks[0] == "endstruct": finish struct; state = none
        else: parse a field line (or `align N`) and append
    case enum:
        if toks[0] == "endenum": finish enum; state = none
        else: parse a variant line and append
if state != none: error "unterminated <block>"
```

Concrete edits:

- `parseStruct` → split into `parseStructHeader` (name + `packed`) and a
  field-line parser; drop the `{` / `}` expectations. `parseField` is unchanged.
- `parseEnum` → split into `parseEnumHeader` and a variant-line parser; drop the
  `{` / `}` expectations.
- `parseFunc`: drop the trailing `c.expect(":")`; the header ends at end of line.
- New error cases: a block opener while a block is open; a terminator with no
  open block; a terminator that does not match the open block; an unterminated
  block at end of file.

## 6. Migration checklist

- `pkg/parser/parser.go` — implementation above.
- `pkg/parser/parser_test.go` — update sources in `TestParseFunc`,
  `TestParsePtrParam`, `TestParseReturnType`, `TestParseOwnParam`,
  `TestParseStruct`, `TestParsePtrStruct`, `TestStructLayout`, `TestParseEnum`,
  and the `TestParseErrors` table; add terminator tests (§7).
- `pkg/codegen/x86_64_test.go` — update the inline sources in the
  `TestCompileAndRun` table (call, fact, ptr, ret_ptr, typed_mem, growable,
  float, f32, simd, simd64, struct, ptr_struct, array, f64_array, bytes, u16_i8,
  unsigned_cmp, packed, enum, preserved_block).
- `pkg/verifier/verifier_test.go` — update the inline sources; `TestExamples`
  already globs `examples/*.arc` and needs no change beyond the examples
  themselves.
- `examples/*.arc` — add `endfunc` to every file that declares functions:
  `call`, `fact`, `ptr`, `ret_ptr`, `own`, `growable`, `float`, `f32`, `simd`,
  `simd64`, `typed_mem`, `struct`, `ptr_struct`, `enum`, `invalid_ptr`,
  `invalid_leak`, `invalid_capacity`, `invalid_borrow`; convert `struct` / `enum`
  declarations to `endstruct` / `endenum`. `hello.arc` and `loop.arc` declare no
  `func` and are unchanged.
- `docs/assembly.md` — grammar §8; the examples in §6, §7, §9, §10, §11.
- `README.md` — the `func` examples and the `struct` / `enum` one-liners.
- `docs/vision.md`, `docs/safety.md`, `docs/registers.md` — confirm no block
  examples need updating.

## 7. Tests to add

- Parse a terminated `func` / `struct` / `enum` and assert the same IR as today.
- Reject: missing terminator at EOF; a terminator with no open block; a
  mismatched terminator (`endstruct` closing a `func`); a block opener inside a
  block.
- Keep `TestExamples` as the end-to-end guard over `examples/`.
- `gofmt`-clean, `go vet`-clean, `go test ./...` green.

## 8. Compatibility

- This is a breaking change to the text format. The project is pre-1.0, so a
  hard switch (update all sources) is preferred. Do **not** accept both
  syntaxes: keeping two conventions is exactly the inconsistency being removed.
- No binary, IR or ABI impact; the verifier and the codegen are untouched.

## 9. Out of scope

- Backend instruction-count optimisation (the `%rax` scratch hop, `lea` for
  addition, register coalescing) — a separate codegen task.
- Unrelated stale prose (e.g. the README still says `r0..r15` in its opening
  paragraphs while the register model is now `r*` / `s*` / `v*`) — a separate
  documentation cleanup.

## 10. Open questions

1. Terminator spelling: `endfunc` (recommended) vs a single `end` vs `end func`.
2. Keep the `func` trailing colon or drop it (recommend drop, so `:` is only a
   label / annotation separator).
3. Fields one per line vs comma-separated (recommend allowing both).
