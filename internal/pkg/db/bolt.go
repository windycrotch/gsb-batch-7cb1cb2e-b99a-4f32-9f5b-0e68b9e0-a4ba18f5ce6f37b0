package db

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/pkg/errors"
	"github.com/sfluor/musig/internal/pkg/model"
	"go.etcd.io/bbolt"
)

var _ Database = &BoltDB{}

// BoltDB implements the Database interface using a bolt database
type BoltDB struct {
	*bbolt.DB
	fingerprintBucket []byte
	songBucket        []byte
	// songMetaBucket stores per song metadata: songID => songMeta
	// (content hash + list of fingerprint keys owned by the song).
	songMetaBucket []byte

	// failHook is used by tests to force a transaction to fail after the
	// metadata of a song has been updated, in order to exercise rollback.
	// It is nil outside of tests.
	failHook func(songID uint32) error
}

// songMeta is the metadata associated with a stored song.
type songMeta struct {
	// contentHash is the hash of the audio file content at the time of the
	// last successful load.
	contentHash []byte
	// keys holds every fingerprint bucket key that contains a TableValue
	// owned by the song. It is used to atomically retire the old fingerprint
	// set when the content of the song changes.
	keys [][]byte
}

// NewBoltDB returns a new bolt database
func NewBoltDB(path string) (*BoltDB, error) {
	db, err := bbolt.Open(path, 0666, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating bolt db with path:%s", path)
	}

	boltDB := &BoltDB{
		DB:                db,
		fingerprintBucket: []byte("fingerprint"),
		songBucket:        []byte("song"),
		songMetaBucket:    []byte("songMeta"),
	}

	// Create buckets
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(boltDB.fingerprintBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(boltDB.songBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(boltDB.songMetaBucket); err != nil {
			return err
		}
		return nil
	})

	return boltDB, errors.Wrap(err, "error creating buckets")
}

// Get retrieves the provided keys' values from the bolt file
func (db *BoltDB) Get(keys []model.EncodedKey) (map[model.EncodedKey][]model.TableValue, error) {
	res := make(map[model.EncodedKey][]model.TableValue, len(keys))

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.fingerprintBucket)

		for _, k := range keys {
			raw := b.Get(k.Bytes())
			if len(raw) == 0 {
				continue
			}
			val, err := model.ValuesFromBytes(b.Get(k.Bytes()))
			if err != nil {
				return errors.Wrapf(err, "wrong record stored: %v", raw)
			}

			res[k] = val
		}

		return nil
	})

	return res, errors.Wrap(err, "an error occurred when reading from bolt")
}

// Set stores the list of (key, value) into the bolt file.
// Existing values for a given key are preserved: duplicates (same encoded
// TableValue) are skipped so storing the same fingerprint twice does not
// inflate the records.
func (db *BoltDB) Set(batch map[model.EncodedKey]model.TableValue) error {
	err := db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.fingerprintBucket)

		for k, v := range batch {
			rawKey := k.Bytes()
			rawVal := v.Bytes()

			existing := b.Get(rawKey)
			if containsValue(existing, rawVal) {
				continue
			}

			if err := b.Put(rawKey, append(existing, rawVal...)); err != nil {
				return errors.Wrapf(err, "error setting (key: %v, val: %v)", k, v)
			}
		}
		return nil
	})

	return errors.Wrap(err, "an error occurred when writing to bolt")
}

// StoreSong registers the given song name and atomically replaces the song's
// fingerprint set with the provided one. All the work happens in a single bolt
// transaction so that:
//   - a song name can never be mapped to an ID without fingerprints,
//   - a failed write leaves the previous fingerprint set intact,
//   - concurrent loads of the same song name share the same song ID.
//
// If the content hash matches the one stored during the previous successful
// load the fingerprint set is left untouched (idempotent reload) and changed
// is false.
func (db *BoltDB) StoreSong(name string, contentHash []byte, fingerprint map[model.EncodedKey]model.TableValue) (uint32, bool, error) {
	var songID uint32
	var changed bool

	err := db.Update(func(tx *bbolt.Tx) error {
		songB := tx.Bucket(db.songBucket)
		metaB := tx.Bucket(db.songMetaBucket)
		fprB := tx.Bucket(db.fingerprintBucket)

		// Lookup the song ID by name inside the same transaction: bolt
		// serializes writers, so two concurrent loads cannot create two
		// different IDs for the same name.
		rawID := findSongIDByName(songB, name)

		var meta songMeta
		if rawID != nil {
			songID = btoi(rawID)
			if rawMeta := metaB.Get(rawID); len(rawMeta) > 0 {
				meta = decodeSongMeta(rawMeta)
			}
			// Unchanged content: nothing to do, the load is a no-op.
			if bytes.Equal(meta.contentHash, contentHash) {
				changed = false
				return nil
			}
		} else {
			// New song: allocate a fresh ID.
			id, err := songB.NextSequence()
			if err != nil {
				return errors.Wrap(err, "error getting next sequence")
			}
			songID = uint32(id)
			rawID = itob(songID)
			if err := songB.Put(rawID, []byte(name)); err != nil {
				return errors.Wrap(err, "error setting song")
			}
		}

		// Stamp every table value with the resolved song ID.
		stamped := make(map[model.EncodedKey]model.TableValue, len(fingerprint))
		for k, v := range fingerprint {
			v.SongID = songID
			stamped[k] = v
		}

		// Retire the previous fingerprint set of the song. Each shared key is
		// rewritten without this song's values; keys it owned exclusively are
		// deleted.
		for _, key := range meta.keys {
			raw := fprB.Get(key)
			filtered := removeSongValues(raw, songID)
			if len(filtered) == 0 {
				if err := fprB.Delete(key); err != nil {
					return errors.Wrap(err, "error deleting old fingerprint key")
				}
			} else if err := fprB.Put(key, filtered); err != nil {
				return errors.Wrap(err, "error rewriting fingerprint key")
			}
		}

		// Insert the new fingerprint set.
		newKeys := make([][]byte, 0, len(stamped))
		for k, v := range stamped {
			rawKey := k.Bytes()
			rawVal := v.Bytes()
			existing := fprB.Get(rawKey)
			if containsValue(existing, rawVal) {
				continue
			}
			if err := fprB.Put(rawKey, append(existing, rawVal...)); err != nil {
				return errors.Wrapf(err, "error setting fingerprint (key: %v)", k)
			}
			newKeys = append(newKeys, rawKey)
		}

		newMeta := songMeta{contentHash: contentHash, keys: newKeys}
		if err := metaB.Put(rawID, encodeSongMeta(newMeta)); err != nil {
			return errors.Wrap(err, "error storing song metadata")
		}

		// Test hook: returning an error here makes bolt rollback the whole
		// transaction (song registration + fingerprint replacement).
		if db.failHook != nil {
			if err := db.failHook(songID); err != nil {
				return err
			}
		}

		changed = true
		return nil
	})

	return songID, changed, errors.Wrap(err, "an error occurred when storing the song")
}

// GetSongID does a song name => songID lookup in the database
func (db *BoltDB) GetSongID(name string) (uint32, error) {
	var id uint32

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.songBucket)

		rawID := findSongIDByName(b, name)
		if rawID == nil {
			return fmt.Errorf("could not find id for song name: %s", name)
		}
		id = btoi(rawID)
		return nil
	})

	return id, errors.Wrap(err, "an error occurred when reading from bolt")
}

// GetSong does a songID => song name lookup in the database
func (db *BoltDB) GetSong(songID uint32) (string, error) {
	var name string

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.songBucket)

		raw := b.Get(itob(songID))
		if len(raw) == 0 {
			return fmt.Errorf("got empty song name")
		}

		name = string(raw)

		return nil
	})

	return name, errors.Wrap(err, "an error occurred when reading from bolt")
}

// SetSong stores a song name in the database and returns it's song ID.
// It first checks if the song is not already stored in the database. The
// lookup and the insertion run in the same serialized write transaction so
// concurrent callers cannot allocate two IDs for the same name.
func (db *BoltDB) SetSong(song string) (uint32, error) {
	var songID uint32

	err := db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.songBucket)

		if rawID := findSongIDByName(b, song); rawID != nil {
			songID = btoi(rawID)
			return nil
		}

		id, err := b.NextSequence()
		if err != nil {
			return errors.Wrap(err, "error getting next sequence")
		}

		songID = uint32(id)
		return errors.Wrap(b.Put(itob(songID), []byte(song)), "error setting song")
	})

	return songID, errors.Wrap(err, "an error occurred when writing to bolt")
}

// FingerprintCount returns the total number of TableValue entries stored in
// the fingerprint bucket.
func (db *BoltDB) FingerprintCount() (int, error) {
	var count int

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.fingerprintBucket)
		return b.ForEach(func(_, v []byte) error {
			n, err := countValues(v)
			if err != nil {
				return err
			}
			count += n
			return nil
		})
	})

	return count, errors.Wrap(err, "an error occurred when counting fingerprints")
}

// SongFingerprintCount returns the number of fingerprint entries owned by the
// given song.
func (db *BoltDB) SongFingerprintCount(songID uint32) (int, error) {
	var count int

	err := db.View(func(tx *bbolt.Tx) error {
		metaB := tx.Bucket(db.songMetaBucket)
		fprB := tx.Bucket(db.fingerprintBucket)

		rawMeta := metaB.Get(itob(songID))
		if len(rawMeta) == 0 {
			return nil
		}
		meta := decodeSongMeta(rawMeta)
		for _, key := range meta.keys {
			raw := fprB.Get(key)
			vals, err := model.ValuesFromBytes(raw)
			if err != nil {
				return err
			}
			for _, v := range vals {
				if v.SongID == songID {
					count++
				}
			}
		}
		return nil
	})

	return count, errors.Wrap(err, "an error occurred when counting the song fingerprints")
}

// SongContentHash returns the content hash stored for the given song.
func (db *BoltDB) SongContentHash(songID uint32) ([]byte, error) {
	var hash []byte

	err := db.View(func(tx *bbolt.Tx) error {
		metaB := tx.Bucket(db.songMetaBucket)
		rawMeta := metaB.Get(itob(songID))
		if len(rawMeta) == 0 {
			return fmt.Errorf("could not find metadata for song id: %d", songID)
		}
		meta := decodeSongMeta(rawMeta)
		hash = meta.contentHash
		return nil
	})

	return hash, errors.Wrap(err, "an error occurred when reading the song metadata")
}

// HashContent returns the sha256 digest of the given content, exposed as a
// small helper so callers do not have to pick the hash function themselves.
func HashContent(content []byte) []byte {
	h := sha256.Sum256(content)
	return h[:]
}

func findSongIDByName(b *bbolt.Bucket, name string) []byte {
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if string(v) == name {
			return k
		}
	}
	return nil
}

// containsValue reports whether the concatenated TableValue records in raw
// already contain the given 8-byte record.
func containsValue(raw, val []byte) bool {
	for i := 0; i+model.TableValueSize <= len(raw); i += model.TableValueSize {
		if bytes.Equal(raw[i:i+model.TableValueSize], val) {
			return true
		}
	}
	return false
}

// removeSongValues drops every TableValue record owned by songID from the
// concatenated records in raw.
func removeSongValues(raw []byte, songID uint32) []byte {
	res := make([]byte, 0, len(raw))
	for i := 0; i+model.TableValueSize <= len(raw); i += model.TableValueSize {
		record := raw[i : i+model.TableValueSize]
		if binary.LittleEndian.Uint32(record[4:]) == songID {
			continue
		}
		res = append(res, record...)
	}
	return res
}

func countValues(raw []byte) (int, error) {
	if len(raw)%model.TableValueSize != 0 {
		return 0, fmt.Errorf("error wrong size for value: %d, expected a multiple of %d", len(raw), model.TableValueSize)
	}
	return len(raw) / model.TableValueSize, nil
}

// encodeSongMeta serializes song metadata as:
// hash length (2 bytes LE) | hash | key count (4 bytes LE) | keys (4 bytes LE each)
func encodeSongMeta(meta songMeta) []byte {
	buf := make([]byte, 0, 2+len(meta.contentHash)+4+len(meta.keys)*4)

	tmp := make([]byte, 4)
	binary.LittleEndian.PutUint16(tmp, uint16(len(meta.contentHash)))
	buf = append(buf, tmp[:2]...)
	buf = append(buf, meta.contentHash...)

	binary.LittleEndian.PutUint32(tmp, uint32(len(meta.keys)))
	buf = append(buf, tmp...)
	for _, k := range meta.keys {
		buf = append(buf, k...)
	}
	return buf
}

func decodeSongMeta(raw []byte) songMeta {
	var meta songMeta
	if len(raw) < 2 {
		return meta
	}

	hashLen := int(binary.LittleEndian.Uint16(raw[:2]))
	if 2+hashLen > len(raw) {
		return meta
	}
	meta.contentHash = append([]byte(nil), raw[2:2+hashLen]...)

	rest := raw[2+hashLen:]
	if len(rest) < 4 {
		return meta
	}
	n := int(binary.LittleEndian.Uint32(rest[:4]))
	rest = rest[4:]

	meta.keys = make([][]byte, 0, n)
	for i := 0; i < n && 4 <= len(rest); i++ {
		key := make([]byte, 4)
		copy(key, rest[:4])
		meta.keys = append(meta.keys, key)
		rest = rest[4:]
	}
	return meta
}

func itob(s uint32) []byte {
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, s)
	return raw
}

func btoi(raw []byte) uint32 {
	return binary.LittleEndian.Uint32(raw)
}
