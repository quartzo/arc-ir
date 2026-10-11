# arc-ir

**Arena Register Code** — a small, statically verified, flat-register
intermediate representation that compiles straight to x86_64 assembly.

ARC is deliberately *not* SSA and does *not* go through LLVM IR. It models a
real machine: a fixed file of 16 registers (`r0..r15`) that instructions read
and write in place, and direct jumps between labeled blocks. The compiler
front end is written in Go and mirrors the eBPF verifier: untrusted programs
(often produced by an LLVM/LLM) are *proven* memory-safe by an abstract
interpreter before any machine code is emitted.

> **Docs:** [Vision](docs/vision.md) · [Safety contract](docs/safety.md) ·
> [Assembly reference](docs/assembly.md) · [AGENTS.md](AGENTS.md)

```
 Source (.arc)   →   Parser (Go)   →   ARC assembly   →   Verifier (Go)   →   x86_64 asm   →   Binary
      text            text/scanner      r0..r15,           abstract           GNU as
      / LLM                             jmp/branch         interpretation     + C runtime
```

Going straight to assembly (instead of LLVM IR) is intentional. LLVM IR
*requires* SSA and phi nodes; recovering SSA from a flat register machine would
mean reimplementing mem2reg inside this compiler — abstracting only to
de-abstract. ARC keeps the same topology as the machine it targets, so
verification and code generation stay simple and direct.

## Status

| Phase | Component | Status |
|-------|-----------|--------|
| 1 | Flat-register IR + CFG (`pkg/ir`) | ✅ |
| 2 | eBPF-style verifier (`pkg/verifier`) | ✅ |
| 3 | `.arc` syntax parser (`pkg/parser`) | ✅ |
| 4 | x86_64 GNU assembly backend (`pkg/codegen`) | ✅ |
| 4 | C support runtime (`pkg/runtime`) | ✅ |
| — | Register allocator: ARC regs → physical regs + register area | ✅ |
| — | Control flow: jumps, branches, loops | ✅ (with range refinement) |
| — | Functions: `CALL`/`RET`, recursion, args in `r1`–`r4` | ✅ |
| — | Pointer parameters (`ptr(...)`) with interprocedural size checks | ✅ |
| — | Non-integer returns: `-> arena`, `-> ptr(...)` | ✅ |
| — | Arena ownership: linear references, leak/borrow checks, `own` params | ✅ |
| — | Typed memory cells: pointers survive a `STORE`/`LOAD` round-trip | ✅ |
| — | Arena modes: fixed (capacity proven) and growable (chunked, abort-on-OOM) | ✅ |
| — | Base types: `f64`/`f32` floats in a separate vector register file (`v0`–`v15`) | ✅ |
| — | SIMD vectors: `f32x4`/`f64x2`/`i32x4`/`i64x2` packed arithmetic (type-level) | ✅ |
| — | Structs: named layouts, field access, definite initialization, `ptr(Struct)` params | ✅ |
| — | Arrays: `[T; N]` regions (`ptr([T; N])`), element-typed loads/stores | ✅ |
| — | Variable-width memory: `i8`…`i64`/`u8`…`u64` integer types, typed 1/2/4/8-byte loads/stores | ✅ |
| — | Struct layout: natural alignment, `packed struct`, per-field `align N` | ✅ |
| — | Signed/unsigned comparison: `JLT`/`JGT` lower to `jl`/`jb` by operand type | ✅ |
| — | Enums: tagged unions (`VARIANT`/`JV`) with active-variant tag refinement | ✅ |
| — | Register blocks: scratch `r0..r31` (volatile) and preserved `s0..s31` (callee-saved) | ✅ |
| — | Physical/area mapping: physical registers + fixed area via a reserved base register | ✅ |

## Layout

```
arc-compiler/
├── cmd/arcc/            # CLI driver (parse → verify → asm → assemble → run)
├── pkg/ir/              # registers, instructions, memory operands, CFG
├── pkg/parser/          # lexer/parser for the .arc syntax (text/scanner)
├── pkg/verifier/        # abstract interpretation: types, ranges, arenas
├── pkg/codegen/         # x86_64 GNU assembly backend
├── pkg/runtime/         # C runtime (arena bump allocator, print), embedded
├── docs/                # assembly reference (docs/assembly.md)
├── examples/            # valid and invalid .arc programs
└── go.mod
```

## The register file

The general-purpose file has two blocks: **scratch** `r0..r31` (volatile across
a call) and **preserved** `s0..s31` (callee-saved). The vector file is `v0..v15`.
Registers are plain 64-bit machine registers with no types of their own — the
verifier tracks their abstract type per program point. A value that must survive
a call lives in a preserved register. See [`docs/registers.md`](docs/registers.md).

The backend assigns each *used* general-purpose register either a physical
register or a slot in a fixed **register area**, addressed through a reserved
base register (`%rbp`):

| block     | physical registers                  | in the area |
|-----------|-------------------------------------|-------------|
| scratch   | `%rcx`, `%rdx`, `%rsi`, `%rdi`, `%r8` (caller-saved) | the rest |
| preserved | `%rbx`, `%r12`, `%r13`, `%r14`, `%r15`, `%r9` | the rest |
| — | `%r10`, `%r11`, `%rax` reserved scratch; `%rsp` stack pointer; `%rbp` area base | — |

A call preserves the preserved registers (the prologue pushes the callee-saved
physical ones and backs up the others it writes). Vector registers spill to the
stack. See [`docs/registers.md`](docs/registers.md) for why the physical counts
are 5/6 rather than the 5/10 the model targets.

## Instruction set

| Instruction | Meaning |
|-------------|---------|
| `rdst = <imm>` / `rdst = rsrc` | load immediate / copy |
| `rdst = ARENA_CREATE <tier>, <size>` | create an arena |
| `rdst = ARENA_ALLOC rsrc, <[T; N] | Struct>` | allocate a typed region inside an arena |
| `ARENA_DESTROY rsrc` | free an arena and invalidate its pointers |
| `rdst = LOAD [mem]` | read a value (width from the pointed-to type) |
| `STORE [mem], rsrc` | write a value (width from the pointed-to type) |
| `VARIANT r, Enum.Variant` | select a union variant (writes the tag) |
| `JV r, Enum.Variant, label` | branch on the active variant, refining it |
| `rdst = ADD rsrc, (<reg> | <imm>)` | also `SUB`, `MUL` |
| `PRINT rsrc` | print an integer |
| `RET rsrc` | return (becomes the process exit code) |
| `JMP label` | unconditional jump |
| `J<cc> ra, (<reg> | <imm>), label` | conditional jump: `JEQ`, `JNE`, `JLT`, `JLE`, `JGT`, `JGE` (signed) and `JLTU`, `JLEU`, `JGTU`, `JGEU` (unsigned) |
| `CALL name` | call function `name`; result in `r0` |
| `RET rsrc` | return from the current function (or the process, for `main`) |
| `label:` | basic-block label |

Memory operands are x86 addressing modes verbatim:
`[base]`, `[base + offset]`, `[base + index*scale + offset]` with
`scale ∈ {1,2,4,8}`.

Complete example (`examples/loop.arc`):

```arc
entry:
    r1 = ARENA_CREATE CXL_POOLED, 128
    r2 = ARENA_ALLOC r1, [i64; 16]
    r3 = 0
loop:
    JGE r3, 10, done
    STORE [r2 + r3*8], r3
    r3 = ADD r3, 1
    JMP loop
done:
    r4 = LOAD [r2 + r3*8 - 8]
    PRINT r4
    ARENA_DESTROY r1
    RET r4
```

## Functions

Functions are declared with `func` and called with `CALL`. Arguments are
passed in `r1`–`r4`, the return value in `r0`, and every other register is
preserved across the call (a call is transparent except for `r0`). Functions
may recurse.

`examples/fact.arc`:

```arc
func main:
    r1 = 5
    CALL fact
    PRINT r0
    RET r0

func fact(r1: i64):
    JGT r1, 1, rec
    r0 = 1
    RET r0
rec:
    r2 = SUB r1, 1
    r3 = r1
    r1 = r2
    CALL fact
    r4 = MUL r3, r0
    r0 = r4
    RET r0
```

Parameters are typed: an integer (`i8`…`i64`, `u8`…`u64`),
`arena`, or `ptr(...)` (a data pointer to a region — a struct `ptr(Struct)` or an
array `ptr([T; N])`). The verifier checks each function independently against its
parameter types, and at every call site checks argument types and that a pointer
argument is at least as large as declared. Functions may recurse.

A function may also declare a return type with `->` (`arena` or `ptr(...)`; the
default is `i64`). The verifier checks the value returned at each `RET`
against that type and gives the caller's `r0` the declared type.

## Ownership

Arenas are **linear resources**: `ARENA_CREATE` produces an owned reference,
and every owned reference must be released exactly once on every path before
the function returns. The verifier rejects leaks, destroying a borrowed arena,
and any use of a handle or pointer after its arena has been released.

- An `arena` parameter is **borrowed** by default; the callee may allocate from
  it but not destroy it.
- An `own arena` parameter **transfers ownership** to the callee, which must
  then release or return it. After such a call, the caller's handle and any
  pointers into that arena are dead.
- Returning an `arena` transfers ownership to the caller.
- Data pointers of unknown provenance (returned from a call) are conservatively
  treated as aliasing any arena.

`examples/own.arc`:

```arc
func main:
    r1 = ARENA_CREATE DRAM_LOCAL, 64
    r2 = ARENA_ALLOC r1, 64
    ...
    CALL consume
    RET r4

func consume(r1: own arena):
    ARENA_DESTROY r1
    RET
```

This is what makes use-after-free across calls detectable — the case that plain
modular type checking missed.

## Arenas

An arena is created either **fixed** (`ARENA_CREATE DRAM_LOCAL, 4096`) — the
verifier proves the cumulative allocation never exceeds the capacity, so it
never fails at run time — or **growable**
(`ARENA_CREATE DRAM_LOCAL, growable(4096)`), which adds chunks on demand and
may abort on out-of-memory. Growable arenas trade the stronger guarantee for
flexibility and tiered growth; both are memory-safe.

## Typed memory

Memory is typed by the **region type**: a `ptr(Struct)` region has one typed cell
per field (a pointer stored in a pointer field survives a `STORE`/`LOAD`
round-trip), and a `ptr([T; N])` region is homogeneous (every access yields `T`).
Releasing an arena invalidates typed cells referring to it.

## Arrays and structs

An **array** is a contiguous run of scalar elements, written `[T; N]` with `T`
an integer (`i8`…`i64`, `u8`…`u64`) or a float (`f32`, `f64`). `ARENA_ALLOC r,
[T; N]` allocates it and gives `r` an array pointer; a load through that pointer
yields the element type — an integer element loads into a general register
(zero/sign-extended by its signedness), a float element into a vector register —
and a store writes the element width (`examples/typed_mem.arc`).

A **struct** is a named layout (`struct Rec { a: u8, b: u16, c: u32, d: u64 }`).
By default fields are naturally aligned; `packed struct` removes padding and a
standalone `align N` forces a field's offset. `ARENA_ALLOC r, Node` allocates it;
`r.field` reads a field and `r.field = rsrc` writes one, with the access width
taken from the field type. Field types are a contract — a wrong type or a
too-small pointer is rejected — and reading a field before writing it is rejected
(definite initialization). A layout travels across a call in a `ptr(Struct)`
parameter, so the callee can read and write fields (`examples/struct.arc`,
`examples/ptr_struct.arc`).

## Enums

An **enum** is a tagged union (`enum Value { Int(n: i64), Pair(a: i32, b: i32),
Nil }`). `ARENA_ALLOC r, Value` allocates it; `VARIANT r, Value.Int` selects a
variant and `JV r, Value.Int, label` branches when that variant is active,
**refining** the verifier's knowledge of the active variant. `r.field` reads a
field of the active variant. Reading a field while the variant is unknown, or a
field of a different variant, or a field that was not written, is rejected — the
tag-refinement generalizes eBPF nullability (`examples/enum.arc`).

## The Verifier

The verifier is an abstract interpreter over the control-flow graph. For every
program point it tracks, per register:

```go
type RegState struct {
    Defined   bool
    Type      RegType // Unknown | i8..i64 | u8..u64 | f32 | f64 | Arena | DataPtr
    Min, Max  int64   // integer value range
    ArenaID   ir.Register
    AllocSize int64   // arena capacity or allocation size
    Destroyed bool
}
```

States are propagated along edges and merged at join points until a fixpoint is
reached. Conditional jumps **refine** integer ranges on each edge: after
`JGE r3, 10, done` the fall-through knows `r3 < 10`, so the dynamic access
`[r2 + r3*8]` in `loop.arc` is proven to stay within the 128-byte allocation.

Rejected programs include:

- **Out-of-bounds** — a static or dynamic access whose range cannot be proven
  to fit `[0, AllocSize)`.
- **Use-after-destroy** — touching a pointer whose arena was freed.
- **Double-destroy**, **use of an uninitialized register**, **type misuse**
  (`LOAD` from an integer, `ARENA_ALLOC` on an integer, ...).
- **Allocation exceeding arena capacity**, **non-positive sizes**.

Errors carry the source line, which lets a generating LLM be fed back in a
*self-healing* loop.

## Usage

```bash
go build -o arcc ./cmd/arcc

# Verify without generating code
./arcc verify examples/loop.arc

# Show the generated x86_64 assembly
./arcc emit-asm examples/loop.arc

# Generate assembly, assemble, link with the runtime, and run
./arcc build examples/loop.arc -run
```

Output of `./arcc build examples/loop.arc -run`:

```
wrote loop.s and arc_runtime.c
built loop
9
program exited with status 9      # exit code = `RET` value
```

Invalid examples are rejected before any code is emitted:

```
$ ./arcc verify examples/invalid_uaf.arc
arcc: verify error (line 7): use-after-destroy: r2 points into a destroyed arena

$ ./arcc verify examples/invalid_oob.arc
arcc: verify error (line 6): out-of-bounds: access [r2 + 12] may reach offset 20, exceeding allocation of 16 bytes
```

### Emitted code (excerpt of `loop.s`)

```asm
.L_loop:
    cmpq $10, %r14
    jge .L_done
    movq %r14, 0(%r13,%r14,8)     ; STORE [r2 + r3*8], r3
    movq %r14, %rax
    addq $1, %rax
    movq %rax, %r14
    jmp .L_loop
.L_done:
    movq -8(%r13,%r14,8), %r15    ; LOAD [r2 + r3*8 - 8]
    ...
```

ARC memory operands map one-to-one onto x86 addressing modes, so each
`LOAD`/`STORE` becomes a single `mov`.

### Runtime ABI

Generated code links against four C functions in `arc_runtime.c` (System V
AMD64 ABI):

| Function | Arguments | Returns |
|----------|-----------|---------|
| `arc_arena_create` | `tier` (`%rdi`), `size` (`%rsi`) | arena handle (`%rax`) |
| `arc_arena_alloc` | `arena` (`%rdi`), `size` (`%rsi`) | block base (`%rax`) |
| `arc_arena_destroy` | `arena` (`%rdi`) | — |
| `arc_print` | `value` (`%rdi`) | — |

## Tests

```bash
go test ./...
```

The tests cover CFG construction, parsing, every verification rejection above,
range refinement, and both complete pipelines (emit assembly → `cc` → run →
check output). Tests that need a C compiler/assembler are skipped when it is
not installed.

## Where AI Connects

With a working verifier, the LLM becomes the primary generation tool:

1. The LLM receives a high-level specification.
2. The LLM emits `.arc` assembly.
3. `arcc verify` runs the verifier.
4. On rejection, the message (with line number) is fed back into the LLM's
   context, which fixes the program automatically — a *self-healing* loop.

## Next Steps

- Path-sensitive merge of ownership (fewer conservative rejections).
- Real CXL/persistent-memory arenas via `mmap` in the runtime.
- Register-pressure-aware allocation and peephole optimization.
