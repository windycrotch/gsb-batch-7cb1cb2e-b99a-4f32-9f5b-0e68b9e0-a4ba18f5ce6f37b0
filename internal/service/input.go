package service

import (
	"context"
	"errors"
	"log"

	"github.com/dionysia-dev/dionysia/internal/db"
	"github.com/dionysia-dev/dionysia/internal/db/model"
	"github.com/google/uuid"
)

var (
	ErrFailedAuth = errors.New("input not found")
)

type Input struct {
	ID            uuid.UUID
	Name          string
	Format        string
	Status        string
	StatusMessage string
	AudioProfiles []AudioProfile
	VideoProfiles []VideoProfile
}

type AudioProfile struct {
	InputID uuid.UUID
	Codec   string
	Bitrate int
}

type VideoProfile struct {
	InputID        uuid.UUID
	Codec          string
	Bitrate        int
	MaxKeyInterval int
	Framerate      int
	Width          int
	Height         int
}

type IngestAuth struct {
	Path   uuid.UUID
	Action string
}

type InputHandler interface {
	CreateInput(context.Context, *Input) (Input, error)
	GetInput(context.Context, uuid.UUID) (Input, error)
	DeleteInput(context.Context, uuid.UUID) error
	Authenticate(context.Context, IngestAuth) error
}

type inputHandler struct {
	store       db.InputStore
	coordinator *Coordinator
	outputPath  string
}

// InputOption customizes an inputHandler. It is used to inject an isolated
// lifecycle Coordinator / artifact root (mainly in tests and custom wiring).
type InputOption func(*inputHandler)

// WithInputCoordinator binds the handler to a specific lifecycle Coordinator.
func WithInputCoordinator(c *Coordinator) InputOption {
	return func(h *inputHandler) {
		if c != nil {
			h.coordinator = c
		}
	}
}

// WithInputOutputPath sets the HLS artifact root used for cleanup on delete.
func WithInputOutputPath(path string) InputOption {
	return func(h *inputHandler) {
		if path != "" {
			h.outputPath = path
		}
	}
}

func NewInputHandler(store db.InputStore, opts ...InputOption) InputHandler {
	handler := &inputHandler{
		store:       store,
		coordinator: DefaultCoordinator(),
	}

	for _, opt := range opts {
		opt(handler)
	}

	return handler
}

// artifactRoot resolves the HLS output root, falling back to the process-wide
// configured default at call time (so fx startup ordering cannot freeze an
// empty path).
func (handler *inputHandler) artifactRoot() string {
	if handler.outputPath != "" {
		return handler.outputPath
	}

	return DefaultPlayoutPath()
}

func (handler *inputHandler) CreateInput(ctx context.Context, in *Input) (Input, error) {
	in.ID = uuid.New()
	if in.Status == "" {
		in.Status = StatusActive
	}

	audioProfiles := make([]model.AudioProfile, len(in.AudioProfiles))
	for i, audio := range in.AudioProfiles {
		audioProfiles[i] = model.AudioProfile{
			InputID: in.ID,
			Codec:   audio.Codec,
			Bitrate: audio.Bitrate,
		}
	}

	videoProfiles := make([]model.VideoProfile, len(in.VideoProfiles))
	for i, video := range in.VideoProfiles {
		videoProfiles[i] = model.VideoProfile{
			InputID:        in.ID,
			Codec:          video.Codec,
			Bitrate:        video.Bitrate,
			MaxKeyInterval: video.MaxKeyInterval,
			Framerate:      video.Framerate,
			Width:          video.Width,
			Height:         video.Height,
		}
	}

	err := handler.store.CreateInput(ctx, &model.Input{
		ID:            in.ID,
		Name:          in.Name,
		Format:        in.Format,
		Status:        in.Status,
		StatusMessage: in.StatusMessage,
		AudioProfiles: audioProfiles,
		VideoProfiles: videoProfiles,
	})

	return *in, err
}

func (handler *inputHandler) GetInput(ctx context.Context, id uuid.UUID) (Input, error) {
	in, err := handler.store.GetInput(ctx, id)
	if err != nil {
		return Input{}, err
	}

	audioProfiles := make([]AudioProfile, len(in.AudioProfiles))
	for i, audio := range in.AudioProfiles { //nolint: gocritic // Can't use pointers in models
		audioProfiles[i] = AudioProfile{
			InputID: in.ID,
			Codec:   audio.Codec,
			Bitrate: audio.Bitrate,
		}
	}

	videoProfiles := make([]VideoProfile, len(in.VideoProfiles))
	for i, video := range in.VideoProfiles {
		videoProfiles[i] = VideoProfile{
			InputID:        in.ID,
			Codec:          video.Codec,
			Bitrate:        video.Bitrate,
			MaxKeyInterval: video.MaxKeyInterval,
			Framerate:      video.Framerate,
			Width:          video.Width,
			Height:         video.Height,
		}
	}

	status := in.Status
	if status == "" {
		status = StatusActive
	}

	return Input{
		ID:            in.ID,
		Name:          in.Name,
		Format:        in.Format,
		Status:        status,
		StatusMessage: in.StatusMessage,
		AudioProfiles: audioProfiles,
		VideoProfiles: videoProfiles,
	}, nil
}

func (handler *inputHandler) DeleteInput(ctx context.Context, id uuid.UUID) error {
	deleteFn := func() error {
		err := handler.store.DeleteInput(ctx, id)
		if err != nil && !errors.Is(err, db.ErrNotFound) && !errors.Is(err, ErrInputNotFound) {
			return err
		}

		// Remove any visible HLS output (playlist + segments) for this input.
		// The packaging execution also cleans up after a cancellation; doing
		// it here as well makes delete's "no artifacts left behind" guarantee
		// independent of which side observes the cancellation first.
		if err := RemoveInputArtifacts(handler.artifactRoot(), id.String()); err != nil {
			log.Printf("failed removing artifacts for input %s: %v", id, err)
		}

		return nil
	}

	if handler.coordinator == nil {
		return deleteFn()
	}

	return handler.coordinator.Delete(id, deleteFn)
}

func (handler *inputHandler) Authenticate(ctx context.Context, authData IngestAuth) error {
	_, err := handler.store.GetInput(ctx, authData.Path)
	if errors.Is(err, db.ErrNotFound) {
		return ErrFailedAuth
	}

	return nil
}
