// Package rc is remote control: delivering one person's keyboard and mouse to
// another person's machine.
//
// This is the most dangerous capability in the product. Everything else here
// shows information; this one takes actions, on somebody else's computer,
// under their identity, with their privileges. A screen share that goes wrong
// leaks a document. A remote control session that goes wrong empties a bank
// account, sends mail as the victim, or installs software. The design below
// is shaped by that asymmetry rather than by convenience.
//
// # The consent model
//
// Six rules. They are stated here because they constrain the code in this
// package and in internal/modules/control, and a later reader changing either
// needs to know which properties were deliberate.
//
//  1. CONSENT IS EXPLICIT AND HUMAN. Control begins only when the person at
//     the machine actively grants it. There is no pre-authorisation, no
//     remembered device, no "always allow", and no administrator override.
//     An admin can see that a session exists and can kill it; no role in the
//     system can start one. The reason is that the entire safety of remote
//     control rests on a human at the keyboard deciding, in the moment, that
//     this specific person may act as them. Any mechanism that lets that
//     decision be made in advance, or by somebody else, destroys it.
//
//  2. CONSENT IS BOUNDED IN TIME. A grant carries a deadline and expires on
//     its own. Renewal is another explicit act. Sessions that persist because
//     nobody remembered to end them are how remote access tools become
//     backdoors.
//
//  3. REVOCATION IS UNILATERAL, INSTANT, AND UNINTERCEPTABLE. The controlled
//     user ends the session alone, without the controller agreeing, and the
//     path that does it must never depend on the controller's cooperation,
//     on the network, or on any code the controller can influence. It is the
//     cheapest and most available operation in the system. Everything else
//     may fail closed; this must never fail at all.
//
//  4. VIEW AND CONTROL ARE DIFFERENT GRANTS. Watching a screen and driving it
//     are separate permissions, and escalating from one to the other requires
//     a fresh, explicit consent. A session cannot quietly gain capability it
//     was not granted.
//
//  5. THE CONTROLLED MACHINE IS THE AUTHORITY. The server brokers and audits;
//     it does not authorise. This package refuses to inject unless it holds a
//     currently valid grant of its own. A compromised or buggy server can
//     therefore fail to stop a session, but cannot start one.
//
//  6. EVERY TRANSITION IS ON THE AUDIT CHAIN. Request, grant, denial,
//     escalation, expiry, revocation and end are all recorded on the
//     tamper-evident log, with both identities. Remote control is exactly the
//     capability whose abuse must be provable afterwards.
//
// # Why input is not optimised for size
//
// The rest of this codebase counts bytes carefully. This package deliberately
// does not, and the contrast is the point.
//
// Screen frames are idempotent: a wrong or missing frame paints the wrong
// pixels for a moment and the next frame repairs it. Input is the opposite.
// A click delivered at the wrong coordinates is not a visual artefact, it is
// an action taken on the user's behalf that may be irreversible: a confirmed
// dialog, a deleted file, a sent message. There is no repair frame for that.
//
// So every event here carries its own absolute coordinates rather than a
// delta, and a click carries the position it was made at rather than relying
// on a preceding move having arrived. A whole second of dense mouse movement
// costs about 500 bytes, roughly 0.3% of what the screen costs in the same
// second. Buying certainty about where clicks land for a third of one percent
// is not a trade that needs thinking about.
package rc
