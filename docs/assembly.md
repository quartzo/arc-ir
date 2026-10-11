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

- **Word size:** 64 bits. Every register holds a 64-bit value. Memory is
  accessed in **typed widths** of 1, 2, 4 or 8 bytes; a load extends the value
  to 64 bits and a store writes the low bytes (see §4.3).
- **Registers:** 32 scratch general-purpose registers (`r0`–`r31`, volatile
  across a call) and 32 preserved ones (`s0`–`s31`, callee-saved), plus 16
  vector registers (`v0`–`v15`, mapped to XMM) for floats and SIMD.
- **Memory:** byte-addressable, little-endian, addressed through 64-bit
  pointers. All application memory is obtained from **arenas** (see §5).
- **Control:** direct jumps to labels; no indirect jumps or computed targets.

## 3. Registers

| Register | Role |
|----------|------|
| `r0` | return value |
| `r1`–`r4` | first four function arguments |
| `r5`–`r31` | scratch temporaries (volatile across a call) |
| `s0`–`s31` | preserved / callee-saved: a value that must survive a call lives here |
| `v0`–`v15` | vector registers: floats (`f32`/`f64`) and SIMD |

Registers are untyped at the machine level. The verifier assigns each register
an abstract type at every program point — an integer type (`i8`…`i64`,
`u8`…`u64`), `f32`, `f64`, `arena` or `data pointer` — and enforces its correct
use. Only general registers may hold integers, arenas or pointers; floats live
in vector registers.

The backend maps the flat file onto physical x86_64 registers plus a fixed
**register area**: 5 scratch and 6 preserved registers are register-resident and
the rest live in the area, addressed through a reserved base register (`%rbp`);
vector registers map to XMM. See [`registers.md`](registers.md).

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
a signed immediate. Operands must be integers; sub-word integers are promoted to
64 bits for the operation, and the result has type `i64`.

### 4.3 Memory access

| Instruction | Semantics |
|-------------|-----------|
| `rdst = LOAD [mem]` | load a value from `mem` |
| `STORE [mem], rsrc` | store `rsrc` to `mem` |

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
proves that `[offset, offset + width)` lies inside the allocation.

The **width and type of the access come from the pointed-to type**, so there is
no width suffix on `LOAD`/`STORE`:

- a `ptr([T; N])` array pointer yields element type `T` (width `sizeof(T)`);
- a `ptr(Struct)` struct pointer yields the type of the field at the offset
  (use `r.field` for field access).

An integer load zero-extends (`u*`) or sign-extends (`i*`) into the 64-bit
register; an `f32`/`f64` load targets a vector register. A store writes the low
`width` bytes; an integer store into an integer cell truncates.

### 4.4 Arena operations

Arenas are the unit of allocation and lifetime. An arena is created with a
memory tier and a capacity; allocations are bump-allocated from it. Destroying
an arena invalidates every pointer derived from it.

| Instruction | Semantics |
|-------------|-----------|
| `rdst = ARENA_CREATE <tier>, <size>` | create a **fixed** arena of `size` bytes |
| `rdst = ARENA_CREATE <tier>, growable(<chunk>)` | create a **growable** arena whose chunks are `chunk` bytes |
| `rdst = ARENA_ALLOC rsrc, [T; N]` | allocate an array of `N` elements of type `T` (`i8`…`i64`, `u8`…`u64`, `f32`, `f64`); `rdst` is an array pointer |
| `rdst = ARENA_ALLOC rsrc, <Struct | Enum>` | allocate `sizeof(...)` and give `rdst` the struct or enum type |
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
| `PRINT rsrc` | print the integer `rsrc` (decimal, newline-terminated) |

### 4.7 Float

| Instruction | Semantics |
|-------------|-----------|
| `vdst = <float literal>` | `vdst = literal` (`f64`) |
| `vdst = f32(<literal>)` | `vdst = literal` (`f32`) |
| `vdst = FADD va, vb` | `vdst = va + vb` (also `FSUB`, `FMUL`, `FDIV`) |
| `vdst = ITOF rsrc` | integer to `f64` |
| `vdst = ITOF32 rsrc` | integer to `f32` |
| `rdst = FTOI vsrc` | float to integer (truncating) |
| `PRINTF vsrc` | print a float (newline-terminated) |

Float values live in vector registers (`v0`–`v15`). The arithmetic and
conversion mnemonics carry **no width**: the operand type picks it (`f32` →
`addss`/`cvttss2siq`, `f64` → `addsd`/`cvttsd2siq`). The verifier requires the
two operands of an arithmetic op to have the same type, and rejects a type
mismatch. `f32` and `f64` never mix implicitly. Only `ITOF`/`ITOF32` and the
literals name a width, because there is no operand to infer it from. A float can
never be used as a pointer or an index, so floats do not affect the memory-safety
guarantee.

### 4.8 SIMD

| Instruction | Semantics |
|-------------|-----------|
| `vdst = f32x4(a, b, c, d)` | 128-bit vector of four `f32` (`f32x4(x)` broadcasts) |
| `vdst = f64x2(a, b)` | 128-bit vector of two `f64` |
| `vdst = i32x4(a, b, c, d)` | 128-bit vector of four `i32` |
| `vdst = i64x2(a, b)` | 128-bit vector of two `i64` |
| `vdst = VADD va, vb` | packed add (`VSUB`, `VMUL`; `VDIV` for float kinds) |
| `PRINTV vsrc` | print the lanes, one per line |

The vector kind is **inferred from the operands** (`f32x4`, `f64x2`, `i32x4` or
`i64x2`), so the arithmetic and print mnemonics carry no suffix; only the
literals name their kind. Vectors live in vector
registers; the verifier tracks the vector type (element kind and width) but not
per-lane values, and requires the two operands of an operation to have the same
kind. Packed integer division and packed 64-bit multiply are not available.

### 4.9 Enums

| Instruction | Semantics |
|-------------|-----------|
| `VARIANT r, Enum.Variant` | select a variant: write the tag, reset the payload |
| `JV r, Enum.Variant, label` | branch if the active variant of `r` is `Variant` |

`VARIANT` writes the tag and makes `r`'s active variant known, so the variant's
fields can be written. `JV` refines the active variant along the taken edge (and
clears it along the fall-through), so the taken block can read that variant's
fields. A field access `r.field` is resolved to a typed load/store at the
field's offset (see §11).

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

Memory is typed by the **region type**, not by ad-hoc writes:

- A `ptr(Struct)` region has one typed cell per field, at the field's offset. A
  pointer stored in a pointer field comes back as a data pointer, so a pointer
  survives a `STORE`/`LOAD` round-trip. Field types are a contract: a value of
  the wrong kind (or, for a pointer field, a too-small allocation) is rejected,
  and reading a field before writing it is rejected.
- A `ptr([T; N])` region is homogeneous: every access yields element type `T`.
  An integer element accepts any integer value (the store truncates); a float
  element requires the matching float type.
- A dynamic offset (`[base + index*scale]`) is allowed into an array pointer,
  but not into a struct pointer (name the field instead).
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

func add(r1: i64, r2: i64):
    r0 = ADD r1, r2
    RET r0
```

The calling convention is:

- Arguments are passed in `r1`–`r4`.
- The return value is passed in `r0`.
- The file has two blocks (see [`registers.md`](registers.md)): **scratch**
  (`r0`..`r31`) is volatile — any call destroys the whole scratch block — and
  **preserved** (`s0`..`s31`) is callee-saved. A value that must survive a call
  lives in a preserved register; the backend saves the preserved registers the
  callee uses.
- The entry function is `main`; returning from it exits the process with `r0`
  as the exit status.

Parameters are declared with a type: an integer (`i8`…`i64`, `u8`…`u64`),
`arena`, or `ptr`. A `ptr` parameter carries the
type of the region it points to — either a named struct (`ptr(Struct)`, see §9)
or an array of scalar elements (`ptr([T; N])`, where `T` is `i8`…`i64`,
`u8`…`u64`, `f32` or `f64`, and `N` is a positive element count). An `arena`
parameter is **borrowed** unless prefixed with `own`, which transfers ownership
to the callee (see §7):

```
func read(r1: ptr([i64; 8])):
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

func make(r1: arena) -> ptr([i64; 8]):
    r0 = ARENA_ALLOC r1, [i64; 8]
    RET r0
```

The default return type is `i64`. The verifier checks at every `RET` that
the returned register matches the declared return type (and, for a `ptr`, that
the target matches and the allocation is large enough and still live), and gives
the caller's `r0` that type after the call.

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

A struct is a named layout. Each field is an integer of some width, an arena
handle, or a pointer (all one word):

```
struct Node { next: ptr(Node), val: i64 }
struct Rec  { a: u8, b: u16, c: u32, d: u64 }
```

By default a struct is **naturally aligned**: each field is placed at an offset
aligned to its own size, and `sizeof` is rounded up to the largest field
alignment. A `packed struct` has no padding, and a standalone `align N`
directive before a field forces that field's offset:

```
packed struct P { a: u8, b: u32 }     ; a@0, b@1, sizeof 5
struct Q { a: u8, align 8, b: u8 }    ; a@0, b@8, sizeof 16
```

- `ARENA_ALLOC r, Node` allocates `sizeof(Node)` and gives `r` the struct type.
- `r.field` reads a field; `r.field = rsrc` writes one. Field access is
  resolved by the verifier to a typed load/store at the field's offset (width
  from the field type).
- Field types are a **contract**: writing a value of the wrong kind (or, for a
  `ptr` field, a smaller allocation) is rejected, and reading a field before it
  is written is rejected (definite initialization).
- A `ptr` field names the region it points to, either a struct (`ptr(Node)`) or
  an array (`ptr([i64; 8])`).

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

## 10. Arrays

An array is a contiguous run of scalar elements. `[T; N]` denotes `N` elements
of type `T`, where `T` is an integer (`i8`…`i64`, `u8`…`u64`) or a float (`f32`,
`f64`); its size is `N * sizeof(T)` bytes. A pointer to an array, `ptr([T; N])`,
may be a parameter type, a return type, or a struct field:

```
func first(r1: ptr([i64; 4])) -> i64:
    r0 = LOAD [r1]
    RET r0
```

`ARENA_ALLOC r, [T; N]` allocates the array and gives `r` an array pointer. A
load through an array pointer yields the element type — an integer element loads
into a general register (zero- or sign-extended by the element's signedness) and
a float element into a vector register — and a store writes the element width.
Element `k` lives at offset `k * sizeof(T)`:

```
    r2 = ARENA_ALLOC r1, [u8; 8]
    r3 = 65
    STORE [r2 + 3], r3       ; r2[3] = 65, one byte
    r4 = LOAD [r2 + 3]       ; r4 = r2[3] = 65
```

Array bounds are checked exactly like any other pointer: the access of width
`sizeof(T)` must lie within `N * sizeof(T)` bytes of the allocation.

## 11. Enums

An enum is a tagged union. Each variant names a set of fields (the same scalar,
arena and pointer types as a struct). The layout is a one-byte tag at offset 0
followed by a payload region sized to the largest variant; every variant's
fields live at their offset inside that shared payload:

```
enum Value { Int(n: i64), Pair(a: i32, b: i32), Nil }
```

`ARENA_ALLOC r, Value` allocates `sizeof(Value)` and gives `r` the enum type.
The active variant is tracked by the verifier:

- `VARIANT r, Value.Int` selects a variant: the tag is written and the payload
  becomes uninitialized, so the variant's fields must be written before reading.
- `JV r, Value.Int, label` branches when the active variant is `Int` and
  **refines** the active variant on the taken edge.
- `r.field` reads a field of the active variant; `r.field = rsrc` writes one.
  Reading a field when the variant is unknown (or a field of a different
  variant) is rejected, and so is reading a field before it is written.

A `ptr(Value)` parameter carries the enum type but not the variant; the callee
tests it with `JV`. Because a callee may change the tag, the caller's variant is
forgotten after a call.

```
func twice(r1: ptr(Value)) -> i64:
    JV r1, Value.Int, as_int
    JV r1, Value.Pair, as_pair
    r0 = 0
    RET r0
as_int:
    r0 = r1.n
    r0 = ADD r0, r0
    RET r0
as_pair:
    r2 = r1.a
    r3 = r1.b
    r0 = ADD r2, r3
    RET r0
```


