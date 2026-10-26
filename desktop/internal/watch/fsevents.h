// Shared between the cgo preambles of fsevents.go (which defines the C
// side) and fsevents_callback.go (which exports the Go side): a preamble
// next to an //export may only declare, not define.
#include <stddef.h>
#include <stdint.h>

typedef struct {
	char *path;
	uint32_t flags;
	uint64_t id;
	uint64_t inode;
} cs_event;

extern void watchGoCallback(uintptr_t handle, cs_event *events, size_t n);
