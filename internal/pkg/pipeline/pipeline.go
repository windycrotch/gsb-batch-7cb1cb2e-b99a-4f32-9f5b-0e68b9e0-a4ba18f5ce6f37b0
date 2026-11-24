package pipeline

import (
	"io/ioutil"
	"os"
	"sync"

	"github.com/pkg/errors"
	"github.com/sfluor/musig/internal/pkg/db"
	"github.com/sfluor/musig/internal/pkg/fingerprint"
	"github.com/sfluor/musig/internal/pkg/model"
	"github.com/sfluor/musig/pkg/dsp"
	log "github.com/sirupsen/logrus"
)

// Pipeline is a struct that allows to operate on audio files
type Pipeline struct {
	s   *dsp.Spectrogrammer
	DB  db.Database
	fpr fingerprint.Fingerprinter

	// storeMu serializes ProcessAndStore calls inside the process. The
	// database already serializes its write transactions, the mutex only
	// avoids redundant fingerprint computation and makes the load ordering
	// deterministic for callers.
	storeMu sync.Mutex
}

// NewDefaultPipeline creates a new default pipeline using a Bolt DB, the default fingerprinter and the default spectrogrammer
func NewDefaultPipeline(dbFile string) (*Pipeline, error) {
	db, err := db.NewBoltDB(dbFile)
	if err != nil {
		return nil, errors.Wrapf(err, "error connection to database at: %s", dbFile)
	}
	s := dsp.NewSpectrogrammer(model.DownsampleRatio, model.MaxFreq, model.SampleSize, true)
	fpr := fingerprint.NewDefaultFingerprinter()

	return &Pipeline{
		s:   s,
		DB:  db,
		fpr: fpr,
	}, nil
}

// NewPipeline creates a new pipeline
func NewPipeline(s *dsp.Spectrogrammer, db db.Database, fpr fingerprint.Fingerprinter) *Pipeline {
	return &Pipeline{
		s:   s,
		DB:  db,
		fpr: fpr,
	}
}

// Close closes the underlying database
func (p *Pipeline) Close() {
	p.DB.Close()
}

// Result represents the output of a pipeline
type Result struct {
	Path         string
	CMap         []model.ConstellationPoint
	SongID       uint32
	SamplingRate float64
	Spectrogram  [][]float64
	Fingerprint  map[model.EncodedKey]model.TableValue
}

// ProcessAndStore process the given audio file and store it in the database
// the computed results are returned.
//
// Loading the same file twice is idempotent: when the file content did not
// change the database is not touched and the same, stable song ID is
// returned. When the content of a same-named file changed, the previous
// fingerprint set of the song is retired inside a single database
// transaction and replaced by the new one, so queries can only ever match a
// single version of the song.
func (p *Pipeline) ProcessAndStore(path string) (*Result, error) {
	partial, contentHash, err := p.read(path)
	if err != nil {
		return nil, err
	}

	// Use 0 as a placeholder song ID: the database stamps the stored values
	// with the real (allocated or reused) song ID inside its transaction.
	songFpr := p.fpr.Fingerprint(0, partial.cMap)

	p.storeMu.Lock()
	id, _, err := p.DB.StoreSong(model.SongNameFromPath(path), contentHash, songFpr)
	p.storeMu.Unlock()
	if err != nil {
		return nil, errors.Wrap(err, "error storing song in database")
	}

	// Reflect the real song ID in the returned fingerprint.
	for k, v := range songFpr {
		v.SongID = id
		songFpr[k] = v
	}

	log.Infof("sucessfully loaded %s into the database", path)
	return &Result{
		Path:         path,
		CMap:         partial.cMap,
		SongID:       id,
		SamplingRate: partial.samplingRate,
		Spectrogram:  partial.spectrogram,
		Fingerprint:  songFpr,
	}, nil
}

// Process process the given audio file and returns a Result
func (p *Pipeline) Process(path string) (*Result, error) {
	partial, _, err := p.read(path)
	if err != nil {
		return nil, err
	}

	// Use 0 as the ID
	var id uint32
	songFpr := p.fpr.Fingerprint(id, partial.cMap)

	log.Infof("sucessfully loaded %s into the database", path)
	return &Result{
		Path:         path,
		CMap:         partial.cMap,
		SongID:       id,
		SamplingRate: partial.samplingRate,
		Spectrogram:  partial.spectrogram,
		Fingerprint:  songFpr,
	}, nil
}

type partialResult struct {
	cMap         []model.ConstellationPoint
	samplingRate float64
	spectrogram  [][]float64
}

func (p *Pipeline) read(path string) (*partialResult, []byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "error opening file at %s", path)
	}
	defer file.Close()

	// Hash the raw file content. Reading it twice is cheap for the short
	// samples the pipeline targets and keeps the hashing independent from
	// any audio decoding step.
	content, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "error reading file at %s", path)
	}
	contentHash := db.HashContent(content)

	if _, err := file.Seek(0, 0); err != nil {
		return nil, nil, errors.Wrapf(err, "error rewinding file at %s", path)
	}

	spec, spr, err := p.s.Spectrogram(file)
	if err != nil {
		return nil, nil, errors.Wrap(err, "error generating spectrogram")
	}

	cMap := p.s.ConstellationMap(spec, spr)

	return &partialResult{
		cMap:         cMap,
		samplingRate: spr,
		spectrogram:  spec,
	}, contentHash, nil
}
