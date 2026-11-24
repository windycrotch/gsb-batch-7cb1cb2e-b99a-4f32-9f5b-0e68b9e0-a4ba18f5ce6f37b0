package service

import (
	"context"
	"log"

	"github.com/dionysia-dev/dionysia/internal/config"
	"github.com/dionysia-dev/dionysia/internal/db"
	"github.com/dionysia-dev/dionysia/internal/queue"
	"github.com/google/uuid"
)

type NotificationHandler interface {
	PackageStream(context.Context, uuid.UUID) error
}

type notificationHandler struct {
	client      queue.Client
	store       db.InputStore
	cfg         *config.Config
	coordinator *Coordinator
}

// NotificationOption customizes a notificationHandler.
type NotificationOption func(*notificationHandler)

// WithNotificationCoordinator binds the enqueue path to a lifecycle
// Coordinator so packaging is rejected once a deletion starts.
func WithNotificationCoordinator(c *Coordinator) NotificationOption {
	return func(h *notificationHandler) {
		if c != nil {
			h.coordinator = c
		}
	}
}

func NewNotificationHandler(c queue.Client, store db.InputStore, cfg *config.Config, opts ...NotificationOption) NotificationHandler {
	handler := &notificationHandler{
		client:      c,
		store:       store,
		cfg:         cfg,
		coordinator: DefaultCoordinator(),
	}

	for _, opt := range opts {
		opt(handler)
	}

	return handler
}

func (h *notificationHandler) PackageStream(ctx context.Context, id uuid.UUID) error {
	// Deletion already won in this process: reject before touching the store.
	if h.coordinator != nil && h.coordinator.IsDeleted(id) {
		return ErrInputNotFound
	}

	in, err := h.store.GetInput(ctx, id)
	if err != nil {
		// Preserve the raw store error (e.g. db.ErrNotFound) so callers can
		// classify it with errors.Is instead of receiving a generic failure.
		return err
	}

	// Close the check-then-enqueue window against a concurrent delete that
	// marked the coordinator while GetInput was in flight. The worker still
	// re-validates persistently, so this is an early, clear rejection rather
	// than the only line of defense.
	if h.coordinator != nil && h.coordinator.IsDeleted(id) {
		return ErrInputNotFound
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

	t, err := NewPackageTask(id, Input{
		ID:            in.ID,
		Name:          in.Name,
		Format:        in.Format,
		Status:        in.Status,
		VideoProfiles: videoProfiles,
		AudioProfiles: audioProfiles,
	}, h.cfg)
	if err != nil {
		return err
	}

	info, err := h.client.Enqueue(t)
	if err != nil {
		return err
	}

	log.Printf("Package Stream enqueued: %s %s\n", info.ID, info.Queue)

	return nil
}
