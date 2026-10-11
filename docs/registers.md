# ARC registers

The general-purpose register file has two blocks:

| block     | names         | semantics                |
|-----------|---------------|--------------------------|
| scratch   | `r0`..`r31`   | volatile across a call   |
| preserved | `s0`..`s31`   | callee-saved             |

The vector file is `v0`..`v15` (the x64 line). It is **volatile**: like scratch,
any call destroys every vector register.

## Scratch

Scratch is volatile: **any call destroys the whole scratch block**. Using a
scratch register after a call is an error ("use of destroyed register"). A value
that must survive a call lives in a preserved register (`s*`).

## Preserved

Preserved is callee-saved: a call leaves it unchanged. The backend saves the
preserved registers a function writes; the verifier does not report them
destroyed.

## Spilling across a call

A call may clobber the caller-saved physical registers (the scratch physical
registers, `%r9` and the XMM file). Just before such a call the backend spills,
and just after it reloads, exactly the registers that are **live across it** —
computed by a backward dataflow on the function CFG, so values live across a
loop's back edge are seen. Only a register whose home is caller-saved is
spilled:

- a register resident in the area or in a vector slot lives in this function's
  own frame, which no call can name, so it is never spilled;
- a register in a callee-saved physical home is preserved by the callee (an ARC
  callee pushes it; a C callee must under the ABI), so it is never spilled;
- the registers a call *defines* are not spilled either: their earlier value is
  dead.

This matters most for the runtime helpers (`arc_print`, `arc_arena_create`,
`arc_arena_alloc`, `arc_arena_destroy`) and for `extern` calls, which the
verifier does not treat as clobbering the volatile files. An `extern` call also
spills its argument registers, because it marshals them from canonical storage
(the area or vector slots), not from the physical homes the ABI overwrites.

## Physical mapping

The design targets 5 scratch and 10 preserved physical registers, with the rest
of the 32+32 file living in a fixed memory area addressed through a reserved base
register. A preserved register whose home a call would clobber is saved on the
stack if the function actually uses it.

The x86_64 backend realizes this with the registers the machine has. Its sixteen
general-purpose registers must also cover the stack pointer, the area base
(`%rbp`) and the code generator's scratch, which leaves room for 5 scratch and 6
preserved physical registers:

| block     | physical                           | in the area |
|-----------|------------------------------------|-------------|
| scratch   | 5 (`%rcx %rdx %rsi %rdi %r8`)      | 27          |
| preserved | 6 (`%rbx %r12 %r13 %r14 %r15 %r9`) | 26          |

Scratch gets caller-saved registers (it is volatile, so the verifier lets no
value survive an ARC call in them; the runtime and `extern` calls still spill
the live ones). Preserved gets the callee-saved ones plus `%r9`; the callee backs
up, on the stack, the preserved registers it writes whose home is not already
covered by the prologue pushes. The prologue pushes the callee-saved registers a
function writes, plus `%rbp` (the area base); the frame size is padded so `rsp`
stays 16-byte aligned at a call. The area is a region of one 8-byte slot per
general-purpose register: a register with no physical home lives there, and a
call reaches the same slot from every function through the base register. In the
current single-threaded backend the area is a static `.bss` symbol
(`arc_regsv`); the model calls for it to be per-thread.

## Status

The physical/area mapping is implemented: every general-purpose register the
module uses is assigned either a physical register or an area slot, the area is
addressed through the reserved base register, and a call preserves the preserved
registers (prologue pushes plus stack backups). Across a call the backend spills
only the live caller-saved registers, and the prologue pushes only the
callee-saved registers the function writes. Vector registers still spill to the
stack. The scratch/preserved counts are 5/6 rather than the 5/10 target because
of the x86_64 register budget above.

