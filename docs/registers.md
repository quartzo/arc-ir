# ARC registers

ARC's register file is a **logical** model, independent of any target. The
backend maps it onto the target's physical registers plus a fixed memory area.
The logical model is the same on every target; only the mapping differs. This
is the SLJIT line: few physical registers, a fixed spill area, and an explicit
scratch/preserved split.

The point of a *logical* file is uniformity: the ISA does not depend on the
target having 16 registers. The cost is that the mapping (logical → physical or
area) is a documented rule the reader must know — it is not one-to-one.

## 1. Two roles

Every logical register has one of two roles:

- **scratch** — volatile across a call. A caller must not expect a scratch
  register to survive a call; a callee may use it freely.
- **preserved** — callee-saved. A callee that uses a preserved register must
  leave it as it found it.

The role is a property of the ISA — it is what the verifier and the calling
convention reason about. It is *not* the target's caller-saved/callee-saved;
that is only one way a target realizes the role.

## 2. The file

Per value class (general-purpose, vector), the logical file is:

| role      | logical | physical (proposed) | in the area (proposed) |
|-----------|---------|---------------------|------------------------|
| scratch   | 32      | 5                   | 27                     |
| preserved | 32      | 9                   | 23                     |

- **General-purpose**: 15 usable GP registers on x86-64 (16 minus `%rsp`). One
  is reserved as the **area base** (§4), leaving 14 for scratch+preserved.
- **Vector**: same scheme; the counts are open (x86-64 has 16 XMM).

## 3. Naming

Numeric, with the **role in the name** — no target names (`rax`, `%rbx`, ...).
A name must not encode the target: that would bake x86 into the ISA and lie for
the registers that live in memory. The spelling is open; the model is
*role + class + index*. The existing ABI roles (return register, argument
registers) are layered on top, as they are today.

## 4. The area and the base

Registers not backed by a physical register live in a **fixed memory area**.
Scratch slots may be overwritten by any call (scratch is volatile). A preserved
slot must be saved/restored by a callee that uses it.

The area is addressed through a reserved **base register** (like `%rsp` is for
the stack), so the area can be **per-microthread**: on a context switch the
scheduler swaps the base together with the stack. This is why the base is a
register and not TLS — TLS is per OS thread, and several microthreads share one
OS thread.

## 5. Calling convention

Implemented (general-purpose registers):

- A function **declares the registers it clobbers**: `func f(...) clobbers rA,
  rB:` (after the return type). Everything else is preserved.
- At a call site the verifier marks the callee's clobbers **destroyed**; using
  one is an error.
- The callee **automatically saves and restores** the registers it writes that
  are not declared as clobbers. There is no `saveAll`.

Still to do (see §6): explicit `PUSH`/`POP`, and the logical file of §2.

## 6. Open decisions

1. **Counts** — the physical split (5/9 vs 4/10), the logical counts (32/32),
   and the vector file.
2. **Naming spelling** — the exact tokens for role + class + index.
3. **Area addressability** — how the verifier models a register that is really a
   memory slot (a typed cell? a register with a fixed address?).
4. **PUSH/POP semantics** — the stack region, bounds, and balance rules.
