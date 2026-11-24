package main

import (
	"context"
	"errors"
	"sort"
)

type MediaListing struct {
	CurrentDirectory *StorageDirectory
	Directories      []*StorageDirectory
	Files            []*StorageFile
	Cover            *StorageFile
	AudioTracks      []*StorageFile
}

type MediaLibrary struct {
	store *S3Storage
}

func NewMediaLibrary(store *S3Storage) *MediaLibrary {
	return &MediaLibrary{
		store: store,
	}
}

func (ml *MediaLibrary) findCover(files []*StorageFile) *StorageFile {
	candidates := ScoreCovers(files)
	if len(candidates) == 0 {
		return nil
	}
	// Score is the primary ordering. Equal scores are broken by the fully
	// qualified storage path so the winner never depends on the order S3
	// pagination happened to return candidates in. sort.SliceStable keeps
	// the relative input order for candidates that compare fully equal.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].path < candidates[j].path
	})
	return candidates[0].StorageFile
}

func (ml *MediaLibrary) listArtworkFiles(ctx context.Context, dirs []*StorageDirectory) ([]*StorageFile, error) {
	var candidates []*StorageFile
	for _, dir := range dirs {
		if !IsArtworkDir(dir) {
			continue
		}
		_, files, err := ml.store.List(ctx, dir.path)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, files...)
	}
	return candidates, nil
}

// findArtworkCover scores images stored in recognized artwork subdirectories.
func (ml *MediaLibrary) findArtworkCover(ctx context.Context, dirs []*StorageDirectory) (*StorageFile, error) {
	artworkFiles, err := ml.listArtworkFiles(ctx, dirs)
	if err != nil {
		return nil, err
	}
	return ml.findCover(artworkFiles), nil
}

// findCoverUpwards selects the cover for the directory p.
//
// Search order, stopping at the first level that yields an image:
//  1. images directly inside p (the already loaded current listing),
//  2. images inside recognized artwork subdirectories of p,
//  3. images directly inside every parent directory, nearest first,
//     up to the storage root.
//
// Artwork subdirectories of ancestors are not rescanned: the upward rule
// looks at the parent directories themselves. A storage failure at any
// level other than "directory does not exist" is returned instead of being
// swallowed, so callers can surface a retryable 5xx error.
func (ml *MediaLibrary) findCoverUpwards(
	ctx context.Context,
	p string,
	dirs []*StorageDirectory,
	files []*StorageFile,
) (*StorageFile, error) {
	if cover := ml.findCover(files); cover != nil {
		return cover, nil
	}
	if cover, err := ml.findArtworkCover(ctx, dirs); err != nil || cover != nil {
		return cover, err
	}

	// Parents is ordered from the root down to the nearest parent; walk it
	// in reverse so the closest ancestor wins. Only files directly inside
	// each ancestor are considered.
	parents := NewStorageDirectory(p).Parents()
	for i := len(parents) - 1; i >= 0; i-- {
		parent := parents[i]
		_, parentFiles, err := ml.store.List(ctx, parent.path)
		if err != nil {
			// An ancestor disappearing concurrently is not fatal for the
			// fallback; keep walking towards the root. Transient storage
			// errors must propagate.
			if IsNotFoundError(err) {
				continue
			}
			return nil, err
		}
		if cover := ml.findCover(parentFiles); cover != nil {
			return cover, nil
		}
	}
	return nil, nil
}

// List returns directory listing under the provided path.
func (ml *MediaLibrary) List(ctx context.Context, p string) (*MediaListing, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dirs, files, err := ml.store.List(ctx, p)
	if err != nil {
		return nil, err
	}

	// Find the album cover in the current directory, its artwork
	// subdirectories or any parent directory.
	cover, err := ml.findCoverUpwards(ctx, p, dirs, files)
	if err != nil {
		return nil, err
	}

	// Find audio tracks and separate all other files. Only files belonging
	// to the current directory are iterated, so a cover inherited from a
	// parent can never drag the parent's other files into this listing.
	var tracks []*StorageFile
	var otherFiles []*StorageFile
	for _, f := range files {
		switch {
		case IsAudioFile(f):
			tracks = append(tracks, f)
		case cover != nil && f.path == cover.path:
			// The selected cover is rendered separately and must not show
			// up again in the generic files list.
			continue
		default:
			otherFiles = append(otherFiles, f)
		}
	}

	listing := &MediaListing{
		CurrentDirectory: NewStorageDirectory(p),
		Directories:      dirs,
		Files:            otherFiles,
		Cover:            cover,
		AudioTracks:      tracks,
	}
	return listing, nil
}

// ContentURL returns a public URL to a file under the given path.
func (ml *MediaLibrary) ContentURL(ctx context.Context, p string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := ValidateStoragePath(p); err != nil {
		return "", err
	}
	url, err := ml.store.FileContentURL(ctx, p)
	if err != nil {
		return "", err
	}
	if url == "" {
		// Defensive guard: never report success with an empty redirect
		// target even if a storage implementation returns no error.
		return "", errors.New("failed to sign content URL")
	}
	return url, nil
}
