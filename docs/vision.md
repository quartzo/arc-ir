# Vision

**ARC (Arena Register Code)** aims to be an *assembly that is as safe as
Rust*: a flat, machine-like instruction set whose programs are statically
proven memory-safe, with memory managed by **arenas** and verification
inspired by the **eBPF** verifier.

## The thesis

At the machine level there are no types: 16 registers of 64 bits and a byte
array. Everything the language adds on top of ordinary assembly is the
**static meaning of a word** — what a value denotes, who owns it, how long it
lives. That meaning lives only in the verifier; it is erased at runtime, so it
costs nothing, but it cannot be recovered dynamically. Therefore every place
where a word crosses a boundary where the flow of meaning is interrupted needs
an annotation to re-establish it:

1. a **function argument** — `r1: ptr([i64; 8])`, `r1: ptr(Node)`, `r1: own arena`;
2. a **function return** — `-> arena(4096)`;
3. **memory** — a statically-addressed cell remembers the type stored in it.

The product is the verifier, not the ISA. The ISA is trivial on purpose.

## The differentiator

A program that passes `arcc verify` has no undefined behavior (see
[`safety.md`](safety.md)). Soundness is the differentiator; completeness is
best-effort. Features exist to *grow* what can be proven, never to weaken the
guarantee.

## Design alternatives and where ARC stands

Each dimension below is a real choice; ARC picked one point in each. The
alternatives are recorded so the choices are visible and revisitable.

### Where safety is enforced

- **(chosen) Verifier at load/compile time** — eBPF's model. Zero runtime cost;
  accepts untrusted programs; the whole program is proven before it runs.
- Compiler type system — Rust's model. Precise, but operates on source, not on
  a flat binary.
- Runtime checks — bounds-checked pointers or a GC. Safe with runtime cost.

### Memory management

- **(chosen) Arenas / regions** — bulk lifetime, no per-object free; a pointer
  into a region is valid exactly as long as the region lives. Two modes: a
  **fixed** arena whose capacity is proven at compile time (never fails at run
  time), and a **growable** arena that adds chunks on demand (aborts on OOM,
  like Rust's default allocator).
- **(chosen) Linear references for arena handles** — ownership is tracked and
  transfers are explicit (`own`), so use-after-free across calls is caught.
- Linear/affine types per allocation — Rust-style, more precise, harder to
  express on a flat machine.
- Reference counting / GC — runtime cost; not "assembly".

### Representation of meaning

- **(chosen) Verifier-tracked abstract state** — no runtime tags, no fat
  pointers.
- Tagged values / fat pointers — runtime cost and size overhead.

### Memory width

- **(chosen) Type-driven access width** — the pointed-to type names the width
  (`i8`…`i64`, `u8`…`u64`, `f32`, `f64`), so `LOAD`/`STORE` carry no width
  suffix and there is no untyped memory to guess at. An integer load extends to
  64 bits by its signedness; a store writes the low bytes.
- Explicit width mnemonics (`LOAD8`, `STORE16`, ...) — more surface, and every
  access must restate a width the type already implies.
- Byte-granular addressing only (e.g. `[u8; N]` buffers) — forces byte-at-a-time
  access and loses the element type.

### Control-flow verification

- **(chosen) Fixpoint with conservative merge** — one state per program point.
- State pruning (eBPF) — needed for scale; not implemented.
- Bounded-loop proofs (eBPF) — ARC allows unbounded loops (ranges widen to
  top) because memory safety does not require termination.

### Interprocedural analysis

- **(chosen) Modular, typed signatures** — each function verified against its
  declared types; every call site checked.
- Whole-program effect summaries — more precise, more complex.

### What to import from eBPF

Imported: value ranges, branch refinement, typed memory cells, linear
references/ownership, cumulative arena-capacity checks.

Available, not yet imported: state pruning, bounded-loop proofs, nullability
types, call-depth limits (deliberately declined).

## Non-goals

- Being as strict as the Linux kernel (e.g. banning recursion).
- Guaranteeing termination.
- A general-purpose high-level language.

## Roadmap

1. **Harden the contract**: fuzz/property tests that try to break soundness.
2. State pruning, for scalability.
3. Path-sensitive ownership merge (fewer conservative rejections).
4. Typed cells across function boundaries (typed `ptr` parameters).
5. Real CXL/persistent-memory arenas via `mmap` in the runtime.
