// Package protocol implements the framing spoken between the Go server and the
// Rust media engine. See docs/PROTOCOL.md.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	Version   = 3
	MaxFrame  = 64 * 1024
	header    = 5
	openEarly = 1
)

type Type uint8

const (
	TypeHello   Type = 0x00
	TypeOpen    Type = 0x01
	TypeAudio   Type = 0x02
	TypeClose   Type = 0x03
	TypeSpeech  Type = 0x81
	TypeCaption Type = 0x82
	TypeError   Type = 0x83
	TypeClosed  Type = 0x84
	TypeStats   Type = 0x85
)

type CaptionKind uint8

const (
	CaptionSource             CaptionKind = 0
	CaptionTranslation        CaptionKind = 1
	CaptionSourcePartial      CaptionKind = 2
	CaptionTranslationPartial CaptionKind = 3
)

func (k CaptionKind) String() string {
	if k&1 != 0 {
		return "translation"
	}
	return "source"
}

func (k CaptionKind) Partial() bool { return k&2 != 0 }

type Frame struct {
	Type      Type
	Stream    uint32
	Version   uint16
	Sequence  uint16
	Kind      CaptionKind
	Utterance uint32
	Source    string
	Target    string
	Early     bool
	Text      string
	Packet    []byte
}

var (
	ErrMalformed = errors.New("protocol: malformed frame")
	ErrTooLarge  = errors.New("protocol: frame too large")
)

func (f Frame) Append(dst []byte) ([]byte, error) {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0, byte(f.Type))
	dst = binary.BigEndian.AppendUint32(dst, f.Stream)

	switch f.Type {
	case TypeHello:
		dst = binary.BigEndian.AppendUint16(dst, f.Version)
		dst = append(dst, f.Text...)
	case TypeOpen:
		if len(f.Source) > 255 || len(f.Target) > 255 {
			return nil, ErrTooLarge
		}
		dst = append(dst, byte(len(f.Source)))
		dst = append(dst, f.Source...)
		dst = append(dst, byte(len(f.Target)))
		dst = append(dst, f.Target...)
		var flags byte
		if f.Early {
			flags |= openEarly
		}
		dst = append(dst, flags)
	case TypeAudio:
		dst = binary.BigEndian.AppendUint16(dst, f.Sequence)
		dst = append(dst, f.Packet...)
	case TypeSpeech:
		dst = append(dst, f.Packet...)
	case TypeCaption:
		dst = append(dst, byte(f.Kind))
		dst = binary.BigEndian.AppendUint32(dst, f.Utterance)
		dst = append(dst, f.Text...)
	case TypeError, TypeStats:
		dst = append(dst, f.Text...)
	case TypeClose, TypeClosed:
	default:
		return nil, fmt.Errorf("%w: unknown type 0x%02x", ErrMalformed, uint8(f.Type))
	}

	length := len(dst) - start - 4
	if length > MaxFrame {
		return nil, ErrTooLarge
	}
	binary.BigEndian.PutUint32(dst[start:], uint32(length))
	return dst, nil
}

func Decode(body []byte) (Frame, error) {
	if len(body) < header {
		return Frame{}, ErrMalformed
	}
	f := Frame{Type: Type(body[0]), Stream: binary.BigEndian.Uint32(body[1:5])}
	payload := body[header:]

	text := func(b []byte) (string, error) {
		if !utf8.Valid(b) {
			return "", ErrMalformed
		}
		return string(b), nil
	}

	var err error
	switch f.Type {
	case TypeHello:
		if len(payload) < 2 {
			return Frame{}, ErrMalformed
		}
		f.Version = binary.BigEndian.Uint16(payload)
		f.Text, err = text(payload[2:])
	case TypeOpen:
		var rest []byte
		if f.Source, rest, err = shortString(payload); err != nil {
			return Frame{}, err
		}
		if f.Target, rest, err = shortString(rest); err != nil {
			return Frame{}, err
		}
		if len(rest) != 1 || rest[0]&^openEarly != 0 {
			return Frame{}, ErrMalformed
		}
		f.Early = rest[0]&openEarly != 0
	case TypeAudio:
		if len(payload) < 2 {
			return Frame{}, ErrMalformed
		}
		f.Sequence = binary.BigEndian.Uint16(payload)
		f.Packet = append([]byte(nil), payload[2:]...)
	case TypeSpeech:
		f.Packet = append([]byte(nil), payload...)
	case TypeCaption:
		if len(payload) < 5 || payload[0] > 3 {
			return Frame{}, ErrMalformed
		}
		f.Kind = CaptionKind(payload[0])
		f.Utterance = binary.BigEndian.Uint32(payload[1:5])
		f.Text, err = text(payload[5:])
	case TypeError, TypeStats:
		f.Text, err = text(payload)
	case TypeClose, TypeClosed:
	default:
		return Frame{}, fmt.Errorf("%w: unknown type 0x%02x", ErrMalformed, body[0])
	}
	if err != nil {
		return Frame{}, err
	}
	return f, nil
}

func shortString(b []byte) (string, []byte, error) {
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return "", nil, ErrMalformed
	}
	n := int(b[0])
	if !utf8.Valid(b[1 : 1+n]) {
		return "", nil, ErrMalformed
	}
	return string(b[1 : 1+n]), b[1+n:], nil
}

func Write(w io.Writer, f Frame) error {
	buf, err := f.Append(nil)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// Read returns io.EOF only when the stream ends cleanly between two frames.
func Read(r io.Reader) (Frame, error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(length[:])
	if n < header || n > MaxFrame {
		return Frame{}, ErrTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Decode(body)
}
