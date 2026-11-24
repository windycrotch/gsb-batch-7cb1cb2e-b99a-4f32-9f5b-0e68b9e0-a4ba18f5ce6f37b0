package db

import "github.com/sfluor/musig/internal/pkg/model"

// Database is an interface for storing fingerprint parts in a database and retrieving them
type Database interface {
	Get([]model.EncodedKey) (map[model.EncodedKey][]model.TableValue, error)
	Set(map[model.EncodedKey]model.TableValue) error

	// StoreSong atomically registers (or reuses) the song ID for the given name
	// and replaces the whole fingerprint set associated with the song.
	// The content hash allows making repeated loads of an unchanged file a no-op:
	// when it matches the hash stored during the previous successful load, no
	// fingerprint entry is touched and the existing song ID is returned.
	StoreSong(name string, contentHash []byte, fingerprint map[model.EncodedKey]model.TableValue) (songID uint32, changed bool, err error)

	GetSongID(name string) (songID uint32, err error)
	GetSong(songID uint32) (name string, err error)
	SetSong(name string) (songID uint32, err error)

	// FingerprintCount returns the total number of (key, TableValue) entries
	// stored in the fingerprint bucket.
	FingerprintCount() (int, error)
	// SongFingerprintCount returns the number of fingerprint entries owned by
	// the given song.
	SongFingerprintCount(songID uint32) (int, error)
	// SongContentHash returns the content hash stored for the given song.
	SongContentHash(songID uint32) ([]byte, error)

	Close() error
}
