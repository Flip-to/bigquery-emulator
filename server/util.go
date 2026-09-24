package server

import (
	"math/rand/v2"
)

const alphanum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

const randomIDLen = 27

// randomID uses the runtime-seeded math/rand/v2 source. It used to seed its
// own source with time.Now().UnixNano() ^ pid, exactly as the Go BigQuery
// client seeds the source for its client-side job IDs; a client in the same
// process (the server tests) initialized in the same clock tick then drew the
// same sequence, so the first jobs.query job ID equalled the first jobs.insert
// job ID and was rejected as "already created".
func randomID() string {
	var b [randomIDLen]byte
	for i := 0; i < len(b); i++ {
		b[i] = alphanum[rand.IntN(len(alphanum))]
	}
	return string(b[:])
}
