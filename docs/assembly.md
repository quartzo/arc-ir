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
- **Registers:** 16 general-purpose registers, `r0` through `r15`.
- **Memory:** byte-addressable, little-endian, addressed through 64-bit
  pointers. All application memory is obtained from **arenas** (see §5).
- **Control:** direct jumps to labels; no indirect jumps or computed targets.

## 3. Registers

| Register | Role |
|----------|------|
| `r0` | return value |
| `r1`–`r4` | first four function arguments |
| `r5`–`r15` | general purpose / temporaries |

Registers are untyped at the machine level. The verifier assigns each register
an abstract type (`scalar`, `arena` or `data pointer`) at every program point
and enforces its correct use.

The backend maps ARC registers onto physical x86_64 registers. The mapping
prefers callee-saved registers and spills to the stack under pressure; the
argument and return registers (`r0`–`r4`) are always register-resident.

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
| `rdst = ARENA_CREATE <tier>, <size>` | create an arena of `size` bytes |
| `rdst = ARENA_ALLOC rsrc, <size>` | allocate `size` bytes inside arena `rsrc`; `rdst` is a data pointer |
| `ARENA_DESTROY rsrc` | free arena `rsrc` and invalidate its pointers |

Tiers: `DRAM_LOCAL`, `CXL_POOLED`, `PERSISTENT` (or the integers `0`, `1`, `2`).

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

## 5. Memory model

All pointers passed to `LOAD`/`STORE` must originate from `ARENA_ALLOC`. An
allocation is a contiguous region `[base, base + size)`. The verifier rejects
any access that is not provably inside a live allocation, and any use of a
pointer whose arena has been destroyed.

An arena's memory tier records where its backing store lives (node-local DRAM,
pooled CXL, persistent memory). In the reference runtime every tier shares the
same backing store; the tier is available to a real runtime for placement
decisions.

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

Parameters are declared with a type: `scalar` and `arena` are supported;
`ptr` is planned. The verifier checks each function independently against its
declared parameter types, and checks at every call site that the argument
registers have the declared types. Functions may recurse.

A program with no `func` declaration is treated as a single entry function
named `main`.

## 7. Program structure

```
program    := { function }
function   := "func" ident [ "(" params ")" ] ":" { block }
block      := { instruction }
```

**(draft)** The grammar above is being formalized. Until the first `func`
appears, a program may also be written without any function declaration, in
which case the whole program is the entry function `main`.
