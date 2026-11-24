package cache

import "sheepbao.com/media/av"
import "flag"

var (
	gopNum = flag.Int("gopNum", 1, "gop num")
)

type Cache struct {
	gop      *GopCache
	videoSeq *SpecialCache
	audioSeq *SpecialCache
	metadata *SpecialCache
	sawVideo bool
	sawAudio bool
}

func NewCache() *Cache {
	return &Cache{
		gop:      NewGopCache(*gopNum),
		videoSeq: NewSpecialCache(),
		audioSeq: NewSpecialCache(),
		metadata: NewSpecialCache(),
	}
}

// Ready reports whether enough of the stream has been cached to start feeding
// a newly attached (or migrated) writer: the writer must receive the codec
// configuration followed by a key frame before any other media packet.
//
// For a video stream this means a key frame has been cached. An audio-only
// stream has no key frame concept, so audio is accepted as soon as it is seen.
func (self *Cache) Ready() bool {
	if self.gop.Ready() {
		return true
	}
	return !self.sawVideo && self.sawAudio
}

func (self *Cache) Write(p av.Packet) {
	if p.IsVideo {
		self.sawVideo = true
	}
	if p.IsAudio {
		self.sawAudio = true
	}
	if p.IsMetadata {
		self.metadata.Write(p)
		return
	} else {
		if !p.IsVideo {
			ah, ok := p.Header.(av.AudioPacketHeader)
			if ok {
				if ah.SoundFormat() == av.SOUND_AAC &&
					ah.AACPacketType() == av.AAC_SEQHDR {
					self.audioSeq.Write(p)
					return
				} else {
					return
				}
			}

		} else {
			vh, ok := p.Header.(av.VideoPacketHeader)
			if ok {
				if vh.IsSeq() {
					self.videoSeq.Write(p)
					return
				}
			} else {
				return
			}

		}
	}
	self.gop.Write(p)
}

func (self *Cache) Send(w av.WriteCloser) error {
	if err := self.metadata.Send(w); err != nil {
		return err
	}

	if err := self.videoSeq.Send(w); err != nil {
		return err
	}

	if err := self.audioSeq.Send(w); err != nil {
		return err
	}

	if err := self.gop.Send(w); err != nil {
		return err
	}

	return nil
}
