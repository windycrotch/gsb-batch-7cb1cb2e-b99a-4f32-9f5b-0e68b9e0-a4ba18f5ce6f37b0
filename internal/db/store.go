package db

import (
	"context"
	"errors"

	"github.com/dionysia-dev/dionysia/internal/db/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrNotFound = errors.New("record not found")
)

type InputStore interface {
	CreateInput(context.Context, *model.Input) error
	GetInput(context.Context, uuid.UUID) (model.Input, error)
	DeleteInput(context.Context, uuid.UUID) error
}

// InputStatusUpdater is an optional capability of an InputStore. It persists
// the packaging lifecycle status of an input without removing the record.
//
// It is defined as a separate interface so existing InputStore
// implementations (and test doubles) keep satisfying InputStore unchanged.
type InputStatusUpdater interface {
	UpdateInputStatus(ctx context.Context, id uuid.UUID, status, message string) error
}

type InputStoreDB struct {
	DB *gorm.DB
}

func NewDBInputStore(db *gorm.DB) InputStore {
	return &InputStoreDB{
		DB: db,
	}
}

func (s *InputStoreDB) CreateInput(ctx context.Context, input *model.Input) error {
	return s.DB.WithContext(ctx).Create(input).Error
}

func (s *InputStoreDB) GetInput(ctx context.Context, id uuid.UUID) (model.Input, error) {
	var input model.Input
	err := s.DB.WithContext(ctx).Preload("AudioProfiles").Preload("VideoProfiles").First(&input, "id = ?", id).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.Input{}, ErrNotFound
	}

	return input, err
}

func (s *InputStoreDB) DeleteInput(ctx context.Context, id uuid.UUID) error {
	// Deletion is idempotent: removing an already absent input is not an
	// error, so concurrent or repeated DELETE requests converge on the same
	// result instead of producing an internal error.
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("input_id = ?", id).Delete(&model.AudioProfile{}).Error; err != nil {
			return err
		}

		if err := tx.Where("input_id = ?", id).Delete(&model.VideoProfile{}).Error; err != nil {
			return err
		}

		if err := tx.Delete(&model.Input{}, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}

			return err
		}

		return nil
	})
}

func (s *InputStoreDB) UpdateInputStatus(ctx context.Context, id uuid.UUID, status, message string) error {
	result := s.DB.WithContext(ctx).
		Model(&model.Input{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":         status,
			"status_message": message,
		})

	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}
