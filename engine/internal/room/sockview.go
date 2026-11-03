package room

// Which socket-path cameras each viewer has on screen.
//
// Media over the socket used to go to everyone in the room. At 50 people
// with cameras on, each person was sent all 49 cameras, 4.1 Mbit/s, though
// a gallery shows about ten (the stress run, ARCHITECTURE 6bs): four fifths
// of the bytes were never drawn, and a phone on a slow link fell behind and
// was dropped from the meeting. The WebRTC path has followed the viewport
// from the start; this is the socket path's equivalent.
//
// A viewer that has never said what it shows is sent every camera, so a
// client that does not report keeps working. Audio and screens are not
// filtered: everyone hears every speaker and sees the presentation.

// SetSocketView records what viewer has on screen, keyed by publisher id;
// keys that are not people in the room are ignored (the same report names
// WebRTC track ids). It returns the publishers that have just come into
// view, which need a key frame to start from.
func (r *Room) SetSocketView(viewer string, visible map[string]bool) (became []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.participants[viewer]
	if !ok {
		return nil
	}
	next := make(map[string]bool)
	for id, on := range visible {
		if _, person := r.participants[id]; person && id != viewer {
			next[id] = on
		}
	}
	if len(next) == 0 {
		return nil // nothing about people: not a report of this kind
	}
	for id, on := range next {
		if on && (v.sockView == nil || !v.sockView[id]) {
			became = append(became, id)
		}
	}
	v.sockView = next
	return became
}

// CameraAudience is who should be sent publisher's socket camera: everyone
// else who has it on screen, or who has never said what they show.
func (r *Room) CameraAudience(publisher string) []*Participant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Participant, 0, len(r.participants))
	for id, p := range r.participants {
		if id == publisher {
			continue
		}
		if p.sockView == nil || p.sockView[publisher] {
			out = append(out, p)
		}
	}
	return out
}
