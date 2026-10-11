# ARC registers

The general-purpose register file has two blocks:

| block     | names         | semantics                |
|-----------|---------------|--------------------------|
| scratch   | `r0`..`r31`   | volatile across a call   |
| preserved | `s0`..`s31`   | callee-saved             |

The vector file is unchanged (`v0`..`v15`, the x64 line).

## Scratch

Scratch is volatile: **any call destroys the whole scratch block**. Using a
scratch register after a call is an error ("use of destroyed register"). A value
that must survive a call lives in a preserved register (`s*`).

## Preserved

Preserved is callee-saved: a call leaves it unchanged. The backend saves, in
its frame, the preserved registers a function actually uses; the verifier does
not report them destroyed.

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

Scratch gets caller-saved registers (it is volatile, so nothing has to survive a
call). Preserved gets the callee-saved ones plus `%r9`; the callee backs up, on
the stack, the preserved registers it writes whose home is not already covered
by the prologue pushes. The area is a region of one 8-byte slot per
general-purpose register: a register with no physical home lives there, and a
call reaches the same slot from every function through the base register. In the
current single-threaded backend the area is a static `.bss` symbol
(`arc_regsv`); the model calls for it to be per-thread.

## Status

The physical/area mapping is implemented: every general-purpose register the
module uses is assigned either a physical register or an area slot, the area is
addressed through the reserved base register, and a call preserves the preserved
registers (prologue pushes plus stack backups). Vector registers still spill to
the stack. The scratch/preserved counts are 5/6 rather than the 5/10 target
because of the x86_64 register budget above.

