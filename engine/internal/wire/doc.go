// Package wire implements Aether's binary control protocol.
//
// Design rules, in priority order:
//
//  1. The common case must be one byte. A heartbeat, an ack, a "still here"
//     is 1 byte of payload. Over an unmasked server->client WebSocket frame
//     that is 3 bytes on the wire. The equivalent JSON ping in every
//     competing product is 40-60 bytes plus frame header.
//
//  2. Nothing is spelled twice. Participant IDs, track IDs, and device
//     labels are interned into a session-scoped dictionary on first use.
//     A 36-byte UUID costs 36 bytes exactly once, then 1 byte forever.
//
//  3. Booleans travel as bits, not bytes. A participant state delta packs
//     mute / camera / screenshare / hand / speaking / pinned into one octet.
//
//  4. Numbers are variable width. LEB128 for magnitudes, zigzag for deltas.
//     Timestamps are always deltas against a session epoch, never absolute
//     millisecond wall clocks.
//
//  5. Every frame is self-delimiting and forward compatible. An unknown
//     opcode carries an explicit length so a v1 peer can skip a v2 frame
//     instead of desynchronizing the dictionary.
//
// # Transport requirement
//
// This protocol REQUIRES a reliable, ordered transport. WebSocket over TLS
// qualifies; so does an SCTP data channel in its default reliable ordered
// mode. An unreliable or unordered data channel DOES NOT, and must never be
// used to carry these frames.
//
// The reason is rule 2. The interning dictionary is cross-frame state: a
// handle means "the string at index N", and both peers derive N by counting
// literals in arrival order. Lose one frame carrying a literal and every
// handle afterwards resolves to the wrong string -- silently, plausibly, and
// permanently. Reorder two and the tables diverge just as badly.
//
// What makes that failure mode genuinely dangerous rather than merely
// inconvenient is the cost of recovery. There is no cheap repair for a
// desynchronised dictionary; the only fix is to resynchronise the whole
// table, which is the most expensive operation the protocol has. So loss
// would trigger the single heaviest transmission at exactly the moment the
// path is least able to carry it, and the congestion that caused the loss
// gets worse. That is a documented failure pattern with real measurements
// behind it, not a theoretical concern -- an earlier delta-coded screen
// codec in a sibling project died of precisely this, spending 13-21 MB to
// display one screen after a drop, and had to be replaced wholesale with
// self-contained frames.
//
// The rule that falls out of it is worth stating in general terms, because
// it governs more than this file: never introduce cross-frame state unless
// recovery from its loss is CHEAP. Standard video codecs are permitted to
// have inter-frame dependency precisely because they arrive with decades of
// loss-recovery engineering. A hand-rolled scheme does not, and this
// dictionary is a hand-rolled scheme. It is safe only because the transport
// underneath it guarantees it will never be tested.
//
// If control frames ever need to move to an unreliable channel, interning
// must be disabled on that channel -- pass a nil dictionary, pay the
// literals, and keep every frame self-contained.
package wire
