package room

// One presenter at a time.
//
// Two screens shared at once is two people talking over each other with
// pictures: the stage has to pick one, and whichever it picks, half the room
// is looking at the wrong thing. Zoom's default is "one participant can
// share at a time"; Teams and Google Meet let whoever starts presenting take
// over from whoever was (Meet asks first, "X is presenting. Present
// instead?"). This is that rule, held by the server so no client can opt
// out of it: the latest claim wins, the previous presenter is told to stop,
// and their screen stops being forwarded at once (May refuses "share" to
// anyone but the presenter), whether or not their client listens.

// ClaimPresenter makes id the presenter and returns who it replaced ("" if
// nobody, or if id already was). Claims are serialised by the room lock, so
// two people pressing Share in the same instant resolve deterministically:
// the second one to arrive presents, the first is replaced.
func (r *Room) ClaimPresenter(id string) (replaced string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.presenter == id {
		return ""
	}
	replaced = r.presenter
	r.presenter = id
	return replaced
}

// ReleasePresenter clears the presenter if it is id. Returns whether it was.
func (r *Room) ReleasePresenter(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.presenter != id || id == "" {
		return false
	}
	r.presenter = ""
	return true
}

// Presenter returns who is presenting, or "".
func (r *Room) Presenter() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.presenter
}
