package schedule

import (
	"sort"
	"sync"
	"time"

	"github.com/LogicalOverflow/music-sync/comm"
	"github.com/LogicalOverflow/music-sync/metadata"
	"github.com/LogicalOverflow/music-sync/playback"
	"github.com/LogicalOverflow/music-sync/timing"
)

type serverState struct {
	sender comm.MessageSender

	lyricsProvider   metadata.LyricsProvider
	metadataProvider metadata.Provider

	playlist *playback.Playlist
	volume   float64

	newestSong *comm.NewSongInfo

	pauses      []*comm.PauseInfo
	pausesMutex sync.RWMutex
}

func (ss *serverState) sendVolume(s comm.MessageSender) {
	ss.pausesMutex.RLock()
	volume := ss.volume
	ss.pausesMutex.RUnlock()

	s.SendMessage(&comm.SetVolumeRequest{Volume: volume})
}

func (ss *serverState) setVolume(volume float64) {
	ss.pausesMutex.Lock()
	ss.volume = volume
	ss.pausesMutex.Unlock()
}

// setPlaying synchronizes a playback command with META snapshots. The pauses
// mutex is acquired before the playlist lock, which is the same order used by
// metaSnapshot, so a subscription is linearized either before or after the
// command.
func (ss *serverState) setPlaying(playing bool) {
	ss.pausesMutex.Lock()
	defer ss.pausesMutex.Unlock()

	if ss.playlist != nil {
		ss.playlist.SetPlaying(playing)
	}
}

func (ss *serverState) sendNewestSong(s comm.MessageSender) {
	ss.pausesMutex.RLock()
	newestSong := ss.newestSong
	ss.pausesMutex.RUnlock()

	if newestSong != nil {
		s.SendMessage(newestSong)
	}
}

func (ss *serverState) sendPauses(s comm.MessageSender) {
	_, pauses := ss.metaSnapshot()
	for _, p := range pauses {
		s.SendMessage(p)
	}
}

// metaSnapshot returns a coherent copy of the newest song and the pause
// information a new META subscriber needs. The returned slice is safe to send
// after the lock has been released.
func (ss *serverState) metaSnapshot() (*comm.NewSongInfo, []*comm.PauseInfo) {
	ss.pausesMutex.Lock()
	defer ss.pausesMutex.Unlock()

	var newestSong *comm.NewSongInfo
	if ss.newestSong != nil {
		// Also prune here for states assembled directly in tests or by an
		// older code path that assigned newestSong without setNewestSong.
		ss.pauses = prunePausesForSong(ss.pauses, ss.newestSong.FirstSampleOfSongIndex)

		song := *ss.newestSong
		newestSong = &song
	}

	pauses := make([]*comm.PauseInfo, len(ss.pauses))
	copy(pauses, ss.pauses)

	var currentPlaying *bool
	if ss.playlist != nil {
		playing := ss.playlist.Playing()
		currentPlaying = &playing
	}

	return newestSong, subscriptionPauses(pauses, newestSong, currentPlaying)
}

func (ss *serverState) sendMetaSnapshot(s comm.MessageSender) {
	newestSong, pauses := ss.metaSnapshot()
	if newestSong != nil {
		s.SendMessage(newestSong)
	}
	for _, pause := range pauses {
		s.SendMessage(pause)
	}
}

func (ss *serverState) createClientHandler() func(comm.Channel, comm.MessageSender) {
	return func(c comm.Channel, s comm.MessageSender) {
		switch c {
		case comm.Channel_AUDIO:
			ss.sendVolume(s)
		case comm.Channel_META:
			ss.sendVolume(s)
			ss.sendMetaSnapshot(s)
		}
	}
}

// subscriptionPauses removes epochs before the newest song and canonicalizes
// the remaining history so a client starting in its default playing state can
// derive the server's current state. When currentPlaying is non-nil, a final
// transition is supplied for a command whose playlist toggle has not yet been
// observed/recorded by the streaming loop.
func subscriptionPauses(raw []*comm.PauseInfo, newestSong *comm.NewSongInfo, currentPlaying *bool) []*comm.PauseInfo {
	if newestSong == nil && currentPlaying == nil {
		pauses := make([]*comm.PauseInfo, len(raw))
		copy(pauses, raw)
		return pauses
	}

	start := uint64(0)
	if newestSong != nil {
		start = newestSong.FirstSampleOfSongIndex
	}
	ordered := coalescePauseHistory(raw)

	maxIndex := start
	for _, p := range ordered {
		if p != nil && p.ToggleSampleIndex > maxIndex {
			maxIndex = p.ToggleSampleIndex
		}
	}

	state := true
	next := 0
	for next < len(ordered) && ordered[next].ToggleSampleIndex < start {
		state = ordered[next].Playing
		next++
	}

	pauses := make([]*comm.PauseInfo, 0, len(ordered)+2)
	if next < len(ordered) && ordered[next].ToggleSampleIndex == start {
		// A recorded toggle at the boundary has precedence over the state
		// inherited from the previous song.
		pauses = append(pauses, ordered[next])
		state = ordered[next].Playing
		next++
	} else if !state {
		// The paused state crosses the song boundary. Do not replay the old
		// toggle position; represent the same state at the new song's start.
		boundaryPause := &comm.PauseInfo{Playing: false, ToggleSampleIndex: start}
		pauses = append(pauses, boundaryPause)
		state = false
	}

	// Every later toggle in the current sample epoch can still be relevant for
	// deriving the current song's progress, so preserve the recorded entries.
	for ; next < len(ordered); next++ {
		pause := ordered[next]
		pauses = append(pauses, pause)
		state = pause.Playing
	}

	if currentPlaying != nil && state != *currentPlaying {
		marker := start
		if maxIndex > start {
			marker = maxIndex + 1
		}
		if n := len(pauses); n > 0 && marker <= pauses[n-1].ToggleSampleIndex {
			marker = pauses[n-1].ToggleSampleIndex + 1
		}
		pauses = append(pauses, &comm.PauseInfo{Playing: *currentPlaying, ToggleSampleIndex: marker})
	}

	return pauses
}

func orderedPauseHistory(raw []*comm.PauseInfo) []*comm.PauseInfo {
	ordered := make([]*comm.PauseInfo, 0, len(raw))
	for _, p := range raw {
		if p != nil {
			ordered = append(ordered, p)
		}
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].ToggleSampleIndex < ordered[j].ToggleSampleIndex
	})
	return ordered
}

// coalescePauseHistory removes ambiguity from rapid toggles recorded at the
// same sample index by keeping the last value in causal/append order.
func coalescePauseHistory(raw []*comm.PauseInfo) []*comm.PauseInfo {
	ordered := orderedPauseHistory(raw)
	coalesced := make([]*comm.PauseInfo, 0, len(ordered))
	for _, pause := range ordered {
		if n := len(coalesced); n > 0 && coalesced[n-1].ToggleSampleIndex == pause.ToggleSampleIndex {
			coalesced[n-1] = pause
			continue
		}
		coalesced = append(coalesced, pause)
	}
	return coalesced
}

// prunePausesForSong discards toggles belonging to an earlier sample epoch.
// A toggle at the new song's first sample is part of the new epoch.
func prunePausesForSong(raw []*comm.PauseInfo, songStart uint64) []*comm.PauseInfo {
	ordered := coalescePauseHistory(raw)
	pauses := ordered[:0]
	for _, pause := range ordered {
		if pause.ToggleSampleIndex >= songStart {
			pauses = append(pauses, pause)
		}
	}
	return pauses
}

func toWireLyrics(lyrics []metadata.LyricsLine) []*comm.NewSongInfo_SongLyricsLine {
	wireLyrics := make([]*comm.NewSongInfo_SongLyricsLine, len(lyrics))
	for i, l := range lyrics {
		wireLine := make([]*comm.NewSongInfo_SongLyricsAtom, len(l))
		for j, a := range l {
			wireLine[j] = &comm.NewSongInfo_SongLyricsAtom{
				Timestamp: a.Timestamp,
				Caption:   a.Caption,
			}
		}
		wireLyrics[i] = &comm.NewSongInfo_SongLyricsLine{Atoms: wireLine}
	}
	return wireLyrics
}

func (ss *serverState) createNewSongHandler() func(uint64, string, int64) {
	return func(startSampleIndex uint64, filename string, songLength int64) {
		lyrics := ss.lyricsProvider.CollectLyrics(filename)
		wireLyrics := toWireLyrics(lyrics)

		md := ss.metadataProvider.CollectMetadata(filename)

		newestSong := &comm.NewSongInfo{
			FirstSampleOfSongIndex: startSampleIndex,
			SongFileName:           filename,
			SongLength:             songLength,
			Lyrics:                 wireLyrics,
			Metadata: &comm.NewSongInfo_SongMetadata{
				Title:  md.Title,
				Artist: md.Artist,
				Album:  md.Album,
			},
		}
		ss.setNewestSong(newestSong)
		if ss.sender != nil {
			ss.sender.SendMessage(newestSong)
		}
	}
}

func (ss *serverState) setNewestSong(newestSong *comm.NewSongInfo) {
	ss.pausesMutex.Lock()
	defer ss.pausesMutex.Unlock()

	ss.newestSong = newestSong
	if newestSong != nil {
		ss.pauses = prunePausesForSong(ss.pauses, newestSong.FirstSampleOfSongIndex)
	}
}

func (ss *serverState) createPauseToggleHandler() func(bool, uint64) {
	return func(playing bool, sample uint64) {
		pause := &comm.PauseInfo{
			Playing:           playing,
			ToggleSampleIndex: sample,
		}
		ss.recordPause(pause)
		if ss.sender != nil {
			ss.sender.SendMessage(pause)
		}
	}
}

func (ss *serverState) recordPause(pause *comm.PauseInfo) {
	if pause == nil {
		return
	}

	ss.pausesMutex.Lock()
	defer ss.pausesMutex.Unlock()

	ss.pauses = append(ss.pauses, pause)
	if ss.newestSong != nil {
		ss.pauses = prunePausesForSong(ss.pauses, ss.newestSong.FirstSampleOfSongIndex)
	} else {
		ss.pauses = coalescePauseHistory(ss.pauses)
	}
}

func (ss *serverState) removablePauses() int {
	ss.pausesMutex.Lock()
	defer ss.pausesMutex.Unlock()
	return ss.removablePausesLocked()
}

func (ss *serverState) removablePausesLocked() int {
	if ss.newestSong == nil {
		return 0
	}

	passed := 0
	for i, p := range ss.pauses {
		if p == nil {
			continue
		}
		inPast := ss.newestSong.FirstSampleOfSongIndex < p.ToggleSampleIndex
		if !inPast && p.Playing {
			passed = i
		} else if inPast {
			break
		}
	}
	return passed
}

func (ss *serverState) removeOldPauses() {
	ss.pausesMutex.Lock()
	defer ss.pausesMutex.Unlock()

	removable := ss.removablePausesLocked()
	if removable <= 0 || len(ss.pauses) < removable {
		return
	}

	copy(ss.pauses, ss.pauses[removable:])
	tailStart := len(ss.pauses) - removable
	for i := tailStart; i < len(ss.pauses); i++ {
		ss.pauses[i] = nil
	}
	ss.pauses = ss.pauses[:tailStart]
}

func (ss *serverState) streamMusic() {
	time.Sleep(StreamStartDelay)
	start := timing.GetSyncedTime() + int64(StreamDelay/time.Nanosecond)
	index := int64(0)
	for range time.Tick(StreamChunkTime) {
		low := make([]float64, StreamChunkSize)
		high := make([]float64, StreamChunkSize)

		firstSampleIndex := ss.playlist.Fill(low, high)

		go ss.sender.SendMessage(&comm.QueueChunkRequest{
			StartTime:        start + int64(index)*int64(StreamChunkTime/time.Nanosecond),
			ChunkId:          index,
			SampleLow:        low,
			SampleHigh:       high,
			FirstSampleIndex: firstSampleIndex,
		})
		go ss.sender.SendMessage(&comm.ChunkInfo{
			StartTime:        start + int64(index)*int64(StreamChunkTime/time.Nanosecond),
			FirstSampleIndex: firstSampleIndex,
			ChunkSize:        uint64(StreamChunkSize),
		})
		index++
	}
}
