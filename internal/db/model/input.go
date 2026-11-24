package model

import (
	"github.com/google/uuid"
)

// Lifecycle statuses persisted on an Input. They describe the last known
// packaging state so operators can still inspect a stream whose packaging
// failed (the record is retained until an explicit delete succeeds).
const (
	InputStatusActive    = "active"
	InputStatusPackaging = "packaging"
	InputStatusFailed    = "failed"
	InputStatusDeleting  = "deleting"
)

type Input struct {
	ID            uuid.UUID `gorm:"primary_key;type:uuid"`
	Name          string
	Format        string
	Status        string         `gorm:"type:varchar(32);not null;default:'active'"`
	StatusMessage string         `gorm:"type:text"`
	AudioProfiles []AudioProfile `gorm:"foreignKey:InputID"`
	VideoProfiles []VideoProfile `gorm:"foreignKey:InputID"`
}

type AudioProfile struct {
	ID      uint `gorm:"primaryKey"`
	InputID uuid.UUID
	Input   Input
	Codec   string
	Bitrate int
}

type VideoProfile struct {
	ID             uint `gorm:"primaryKey"`
	InputID        uuid.UUID
	Input          Input
	Codec          string
	Bitrate        int
	MaxKeyInterval int
	Framerate      int
	Width          int
	Height         int
}
