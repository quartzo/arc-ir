# ARC Assembly Reference

> This is the beginning of the ARC (Arena Register Code) assembly reference.
> It documents the instruction set, the register file, the memory model and the
> calling convention. Sections marked **(draft)** are still being specified.

## 1. Overview

ARC is a flat-register, load/store assembly language. It is designed to be a
faithful abstraction of a real 64-bit machine: a small fixed register file,
explicit memory operands and direct jumps. There is no SSA form, no phi nodes
and no virtual register namespace — every instruction reads and writes the
physical-style registers in place.

A program is a sequence of instructions grouped into **basic blocks** by
labels. Instructions execute in order until a jump changes control flow. A
label is a jump target; execution falls through from one block to the next
unless a terminator (`JMP` or `RET`) is reached.

ARC is statically verified before it is assembled. The verifier (see
`pkg/verifier`) tracks the abstract type, value range and arena binding of every
register at every program point, and rejects any program it cannot prove
memory-safe.

## 2. Machine model

- **Word size:** 64 bits. Every register and every memory slot holds a 64-bit
  value. There are no sub-word types.
- **Registers:** 16 general-purpose registers (`r0`–`r15`) and 16 vector
  registers (`v0`–`v15`, mapped to XMM) for floats and SIMD.
- **Memory:** byte-addressable, little-endian, addressed through 64-bit
  pointers. All application memory is obtained from **arenas** (see §5).
- **Control:** direct jumps to labels; no indirect jumps or computed targets.

## 3. Registers

| Register | Role |
|----------|------|
| `r0` | return value |
| `r1`–`r4` | first four function arguments |
| `r5`–`r15` | general purpose / temporaries |
| `v0`–`v15` | vector registers: floats (`f64`) and, later, SIMD |

Registers are untyped at the machine level. The verifier assigns each register
an abstract type at every program point — `scalar`, `f64`, `arena` or
`data pointer` — and enforces its correct use. Only general registers may hold
integers, arenas or pointers; floats live in vector registers.

The backend maps ARC registers onto physical x86_64 registers: general
registers prefer callee-saved ones and spill to the stack under pressure;
vector registers map to XMM. The argument and return registers (`r0`–`r4`) are
always register-resident.

## 4. Instructions

### 4.1 Data movement

| Instruction | Semantics |
|-------------|-----------|
| `rdst = <imm>` | `rdst = imm` |
| `rdst = rsrc` | `rdst = rsrc` |

### 4.2 Arithmetic

| Instruction | Semantics |
|-------------|-----------|
| `rdst = ADD ra, rb` | `rdst = ra + rb` |
| `rdst = ADD ra, imm` | `rdst = ra + imm` |
| `rdst = SUB ra, rb` | `rdst = ra - rb` |
| `rdst = MUL ra, rb` | `rdst = ra * rb` |

Arithmetic wraps modulo 2^64 (two's complement). Operands may be a register or
a signed immediate. All arithmetic operates on scalars.

### 4.3 Memory access

| Instruction | Semantics |
|-------------|-----------|
| `rdst = LOAD [mem]` | load a 64-bit scalar from `mem` |
| `STORE [mem], rsrc` | store the 64-bit scalar `rsrc` to `mem` |

A `mem` operand has the form:

```
[base]
[base + offset]
[base + index*scale]
[base + index*scale + offset]
```

where `base` and `index` are registers, `scale` is one of `1, 2, 4, 8`, and
`offset` is a signed immediate. The effective address is
`base + index*scale + offset`. `base` must be a data pointer; the verifier
proves that `[offset, offset + 8)` lies inside the allocation.

### 4.4 Arena operations

Arenas are the unit of allocation and lifetime. An arena is created with a
memory tier and a capacity; allocations are bump-allocated from it. Destroying
an arena invalidates every pointer derived from it.

| Instruction | Semantics |
|-------------|-----------|
| `rdst = ARENA_CREATE <tier>, <size>` | create a **fixed** arena of `size` bytes |
| `rdst = ARENA_CREATE <tier>, growable(<chunk>)` | create a **growable** arena whose chunks are `chunk` bytes |
| `rdst = ARENA_ALLOC rsrc, <size>` | allocate `size` bytes inside arena `rsrc`; `rdst` is a data pointer |
| `ARENA_DESTROY rsrc` | free arena `rsrc` and invalidate its pointers |

Tiers: `DRAM_LOCAL`, `CXL_POOLED`, `PERSISTENT` (or the integers `0`, `1`, `2`).

A **fixed** arena has a capacity that the verifier proves the cumulative
allocation never exceeds. A **growable** arena starts with one chunk and adds
another whenever the current one is full; the verifier only requires each
allocation to fit in a single chunk. A growable arena may abort on
out-of-memory (a safe failure), whereas a fixed arena never fails at run time.

### 4.5 Control flow

| Instruction | Semantics |
|-------------|-----------|
| `label:` | define a jump target |
| `JMP label` | unconditional jump |
| `J<cc> ra, rb, label` | jump to `label` if the comparison holds |
| `CALL name` | call function `name`; result in `r0` |
| `RET rsrc` | return (from the function, or the process for `main`) |

Condition codes:

| Mnemonic | Condition |
|----------|-----------|
| `JEQ` | `ra == rb` |
| `JNE` | `ra != rb` |
| `JLT` | `ra < rb` (signed) |
| `JLE` | `ra <= rb` (signed) |
| `JGT` | `ra > rb` (signed) |
| `JGE` | `ra >= rb` (signed) |

A conditional jump does **not** end a basic block: on the not-taken path,
execution continues with the next instruction.

### 4.6 Miscellaneous

| Instruction | Semantics |
|-------------|-----------|
| `PRINT rsrc` | print the scalar `rsrc` (decimal, newline-terminated) |

### 4.7 Float

| Instruction | Semantics |
|-------------|-----------|
| `vdst = <float literal>` | `vdst = literal` (`f64`) |
| `vdst = f32(<literal>)` | `vdst = literal` (`f32`) |
| `vdst = FADD va, vb` | `vdst = va + vb` (`f64`; also `FSUB`, `FMUL`, `FDIV`) |
| `vdst = FADD32 va, vb` | `vdst = va + vb` (`f32`; also `FSUB32`, `FMUL32`, `FDIV32`) |
| `vdst = ITOF rsrc` | integer to `f64` |
| `vdst = ITOF32 rsrc` | integer to `f32` |
| `rdst = FTOI vsrc` | `f64` to integer (truncating) |
| `rdst = F32TOI vsrc` | `f32` to integer (truncating) |
| `PRINTF vsrc` | print an `f64` (newline-terminated) |
| `PRINTF32 vsrc` | print an `f32` (newline-terminated) |

Float values live in vector registers (`v0`–`v15`). `f32` and `f64` never mix
implicitly; the verifier enforces that operands match the instruction's width
and that conversions are explicit. A float can never be used as a pointer or an
index, so floats do not affect the memory-safety guarantee.

### 4.8 SIMD

| Instruction | Semantics |
|-------------|-----------|
| `vdst = f32x4(a, b, c, d)` | 128-bit vector of four `f32` (`f32x4(x)` broadcasts) |
| `vdst = f64x2(a, b)` | 128-bit vector of two `f64` |
| `vdst = i32x4(a, b, c, d)` | 128-bit vector of four `i32` |
| `vdst = i64x2(a, b)` | 128-bit vector of two `i64` |
| `vdst = VADD.<kind> va, vb` | packed add (`VSUB`, `VMUL`; `VDIV` for float kinds) |
| `PRINTV.<kind> vsrc` | print the lanes, one per line |

`<kind>` is `f32x4`, `f64x2`, `i32x4` or `i64x2`. Vectors live in vector
registers; the verifier tracks the vector type (element kind and width) but not
per-lane values, and requires the two operands of an operation to have the same
kind. Packed integer division and packed 64-bit multiply are not available.

## 5. Memory model

All pointers passed to `LOAD`/`STORE` must originate from `ARENA_ALLOC`. An
allocation is a contiguous region `[base, base + size)`. The verifier rejects
any access that is not provably inside a live allocation, and any use of a
pointer whose arena has been destroyed.

An arena's memory tier records where its backing store lives (node-local DRAM,
pooled CXL, persistent memory). In the reference runtime every tier shares the
same backing store; the tier is available to a real runtime for placement
decisions.

### Typed cells

Memory written through a **static** offset into a locally-known allocation is
typed: the verifier remembers the type of the value stored at each offset.
Storing a data pointer and loading it back yields a data pointer, not an
unknown scalar, so a pointer survives a `STORE`/`LOAD` round-trip.

- Writing an arena handle into memory is not allowed (only scalars and data
  pointers).
- Dynamic-offset accesses (`[base + index*scale]`) and accesses through a
  pointer of unknown provenance are scalar-only, since the verifier cannot name
  the cell.
- When an arena is released, typed cells referring to it are invalidated, so a
  pointer loaded afterwards is dead.

## 6. Functions and the calling convention

Functions are declared with `func`:

```
func main:
    r1 = 3
    r2 = 4
    CALL add
    PRINT r0
    RET r0

func add(r1: scalar, r2: scalar):
    r0 = ADD r1, r2
    RET r0
```

The calling convention is:

- Arguments are passed in `r1`–`r4`.
- The return value is passed in `r0`.
- All other registers are preserved across a call; a call is transparent to
  the caller except for `r0`.
- The entry function is `main`; returning from it exits the process with `r0`
  as the exit status.

Parameters are declared with a type: `scalar`, `arena` and `ptr`. A `ptr`
parameter carries the size of the allocation it refers to, written
`ptr(<size>)`. An `arena` parameter is **borrowed** unless prefixed with `own`,
which transfers ownership to the callee (see §7):

```
func read(r1: ptr(64)):
    r0 = LOAD [r1]
    RET r0
```

The verifier checks each function independently against its declared parameter
types, and at every call site that the argument registers have the declared
types and that a pointer argument is at least as large as declared. Functions
may recurse.

A function may declare a return type with `->`:

```
func get_arena() -> arena:
    r0 = ARENA_CREATE DRAM_LOCAL, 4096
    RET r0

func make(r1: arena) -> ptr(64):
    r0 = ARENA_ALLOC r1, 64
    RET r0
```

The default return type is `scalar`. The verifier checks at every `RET` that
the returned register matches the declared return type (and, for `ptr(size)`,
that the allocation is large enough and still live), and gives the caller's
`r0` that type after the call.

A program with no `func` declaration is treated as a single entry function
named `main`.

## 7. Ownership and lifetime

Arenas are **linear resources**. `ARENA_CREATE` produces an owned reference,
and every owned reference must be released exactly once on every control-flow
path before the function returns — otherwise the verifier reports a leak.

- `ARENA_DESTROY r` releases the arena in `r`. `r` must be an owned arena;
  destroying a borrowed arena is rejected.
- Releasing an arena invalidates every handle and data pointer to it. Data
  pointers whose arena provenance is unknown (e.g. returned from a call) are
  conservatively treated as aliasing any arena and are invalidated too.
- An `arena` parameter is borrowed: the callee may allocate from it but must
  not destroy it. An `own arena` parameter transfers ownership to the callee,
  which must then release or return it.
- Returning an `arena` transfers ownership to the caller.

```
func main:
    r1 = ARENA_CREATE DRAM_LOCAL, 64   ; owned here
    CALL consume                        ; ownership moves to consume
    RET

func consume(r1: own arena):
    ARENA_DESTROY r1                    ; released here
    RET
```

This is what makes use-after-free across calls detectable: after a call that
consumes an arena, every handle and pointer to it becomes dead.

## 8. Program structure

```
program    := { function }
function   := "func" ident [ "(" params ")" ] ":" { block }
block      := { instruction }
```

**(draft)** The grammar above is being formalized. Until the first `func`
appears, a program may also be written without any function declaration, in
which case the whole program is the entry function `main`.

## 9. Structs

A struct is a named layout. Every field is one 8-byte word, at consecutive
offsets:

```
struct Node { next: ptr(8), val: scalar }
```

- `ARENA_ALLOC r, Node` allocates `sizeof(Node)` and gives `r` the struct type.
- `r.field` reads a field; `r.field = rsrc` writes one. Field access is
  resolved by the verifier to a plain load/store at the field's offset.
- Field types are a **contract**: writing a value of the wrong kind (or, for a
  `ptr` field, a smaller allocation) is rejected, and reading a field before it
  is written is rejected (definite initialization).

A struct layout travels in a `ptr(Struct)` parameter, so the callee can read
and write the fields:

```
func read(r1: ptr(Node)):
    r2 = r1.next
    r0 = LOAD [r2]
    RET r0
```

The caller must pass a pointer of that struct whose fields are all
initialized. Because the callee may write through the pointer, the caller's
view of those cells becomes stale after the call (reads yield scalars until the
fields are stored again).
