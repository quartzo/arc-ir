/*
 * arc_runtime.c - C support runtime for ARC assembly programs.
 *
 * The generated x86_64 code calls these functions. An arena is a list of
 * chunks; allocations bump-allocate within the current chunk. There are two
 * kinds of arena:
 *
 *   - fixed: a single chunk of a given capacity. The verifier proves that the
 *     cumulative allocation fits, so a fixed arena never overflows here.
 *   - growable: allocation starts in a chunk of a given size and adds a new
 *     chunk when the current one is full. Allocation failure aborts (safe
 *     failure, like Rust's default allocator).
 *
 * The functions use the System V AMD64 ABI: first integer argument in %rdi,
 * second in %rsi, return value in %rax. The verifier proves that no program
 * reaches these functions with out-of-bounds or destroyed arenas; the checks
 * here are a defence-in-depth backstop.
 */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

typedef struct ArcChunk {
    struct ArcChunk *next;
    int64_t          used;
    int64_t          size;
    /* `size` bytes of storage follow the header. */
} ArcChunk;

typedef struct {
    ArcChunk *head;
    ArcChunk *cur;
    int64_t   chunk;     /* chunk size (growable) or capacity (fixed) */
    int64_t   tier;
    int32_t   growable;
    int32_t   destroyed;
} ArcArena;

static ArcChunk *arc_new_chunk(int64_t size) {
    ArcChunk *c = (ArcChunk *)malloc(sizeof(ArcChunk) + (size_t)size);
    if (!c) {
        abort();
    }
    c->next = NULL;
    c->used = 0;
    c->size = size;
    return c;
}

static uint8_t *arc_chunk_data(ArcChunk *c) {
    return (uint8_t *)(c + 1);
}

long arc_arena_create(long tier, long size) {
    ArcArena *a = (ArcArena *)malloc(sizeof(ArcArena));
    if (!a) {
        abort();
    }
    a->head = arc_new_chunk(size);
    a->cur = a->head;
    a->chunk = size;
    a->tier = tier;
    a->growable = 0;
    a->destroyed = 0;
    return (long)a;
}

long arc_arena_create_growable(long tier, long chunk) {
    ArcArena *a = (ArcArena *)malloc(sizeof(ArcArena));
    if (!a) {
        abort();
    }
    a->head = NULL;
    a->cur = NULL;
    a->chunk = chunk;
    a->tier = tier;
    a->growable = 1;
    a->destroyed = 0;
    return (long)a;
}

long arc_arena_alloc(long arena, long size) {
    ArcArena *a = (ArcArena *)arena;
    if (!a || a->destroyed) {
        abort();
    }
    if (a->cur == NULL || a->cur->used + size > a->cur->size) {
        if (!a->growable) {
            abort(); /* fixed arena: the verifier proved this cannot happen */
        }
        int64_t csz = a->chunk > size ? a->chunk : size;
        ArcChunk *c = arc_new_chunk(csz);
        if (a->cur) {
            a->cur->next = c;
        } else {
            a->head = c;
        }
        a->cur = c;
    }
    uint8_t *p = arc_chunk_data(a->cur) + a->cur->used;
    a->cur->used += size;
    return (long)p;
}

void arc_arena_destroy(long arena) {
    ArcArena *a = (ArcArena *)arena;
    if (!a) {
        return;
    }
    ArcChunk *c = a->head;
    while (c) {
        ArcChunk *next = c->next;
        free(c);
        c = next;
    }
    a->destroyed = 1;
    free(a);
}

void arc_print(long v) {
    printf("%lld\n", (long long)v);
}
