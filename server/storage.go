package server

type Storage string

const (
	MemoryStorage Storage = "file::memory:?cache=shared"
	TempStorage   Storage = "tmp"

	// tempStoragePragmas make the throwaway TempStorage file skip fsync.
	// Durability is not a goal for a file deleted at shutdown; a crash loses
	// it either way. The journal mode is left to withConcurrencyPragmas, which
	// turns on WAL (and with it concurrent reads) only when the DSN does not
	// set a journal_mode itself.
	tempStoragePragmas = "_pragma=synchronous(off)"
)
