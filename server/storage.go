package server

type Storage string

const (
	MemoryStorage Storage = "file::memory:?cache=shared"
	TempStorage   Storage = "tmp"

	// tempStoragePragmas make the throwaway TempStorage file skip fsync and
	// keep its rollback journal in memory. Durability is not a goal for a
	// file deleted at shutdown; a crash loses it either way.
	tempStoragePragmas = "_pragma=synchronous(off)&_pragma=journal_mode(memory)"
)
