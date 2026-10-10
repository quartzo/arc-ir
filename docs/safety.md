# ARC safety contract

This document states exactly what `arcc verify` guarantees, and what it does
not. It is the project's core promise; everything else (the ISA, the backends,
the syntax) exists to serve it.

## The guarantee

If `arcc verify` accepts a program, then running the emitted x86_64 code
cannot cause undefined behavior:

- no out-of-bounds load or store,
- no use of a pointer into a released arena (use-after-free),
- no double release of an arena,
- no use of an uninitialized register,
- no allocation that exceeds an arena's declared capacity (cumulative, across
  the whole program, including through function calls),
- no type misuse (loading through a scalar, storing an arena handle in memory,
  transferring a borrowed arena as owned, ...).

The guarantee is **whole-program**: it holds across function calls, because
functions are verified against typed signatures and every call site is checked
against them.

Arena capacity comes in two modes. A **fixed** arena has a capacity the
verifier proves is never exceeded, so it never fails at run time. A
**growable** arena adds chunks on demand and may abort on out-of-memory — a
safe failure (like Rust's default allocator), not undefined behavior. Either
way, no allocation overflows memory.

## Sound, not complete

The verifier is **sound**: it never accepts a program that violates the
guarantee. It is **not complete**: it may reject a program that is in fact
safe. A rejection means "cannot prove", not "unsafe". Soundness is the
contract; completeness is best-effort.

## What is NOT guaranteed

- **Termination.** The verifier proves memory safety, not that the program
  halts. Loops and recursion may run forever.
- **No leaks.** Leak-freedom is checked where the analysis can see it (every
  owned arena must be released or returned on every path), but it can be
  missed at some control-flow joins. Leaks are safe, not undefined behavior —
  Rust makes the same allowance for `mem::forget`.
- **Bounded stack.** Deep recursion may overflow the machine stack; the
  verifier does not bound call depth (unlike the Linux eBPF verifier).

## The analysis, in one paragraph

The verifier is an abstract interpreter over the control-flow graph. Per
program point it tracks, for each of the 16 registers, an abstract value:
scalar with a signed range, arena handle (with reference identity, capacity and
ownership), or data pointer (with arena, allocation identity and size). It
also tracks the type of statically-addressed memory cells and the bytes
allocated so far from each arena. States flow along edges and merge at joins (a
conservative union). Conditional jumps refine scalar ranges on each edge.
Arenas are linear references: ownership is tracked and transfers are explicit.

## Non-goals / known threats

- **Bugs in the verifier itself.** The range arithmetic and the merge are
  hand-written and not formally proven. `FuzzVerify` checks robustness (no
  panics on arbitrary input), not soundness.
- **The runtime.** The C support runtime is trusted. The verifier proves the
  runtime's checks are never needed; the checks remain as a backstop.
- **Concurrency.** ARC is single-threaded; there are no data races to reason
  about yet.

## How to change the verifier safely

Any change to the verifier must state whether it **strengthens** or
**preserves** the guarantee above. A change that weakens it is a regression,
even if it makes more programs verify. New features come after gaps are closed,
not before.
