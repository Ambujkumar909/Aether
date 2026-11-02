// Package settings is the deployment-wide policy an administrator sets once,
// and which every meeting then starts from.
//
// The distinction that matters is between a DEFAULT and a CEILING, and both
// products this borrows from get it right for a reason. A default is what a
// new meeting starts as, and a host may change it. A ceiling is what a host
// may not exceed, whatever they click. Conflating the two produces an admin
// console full of switches that hosts quietly override, which is worse than
// having no console: it tells the person responsible for the deployment that
// something is enforced when it is not.
//
// So each policy below says which it is, and the enforcement lives where the
// action happens rather than in the interface that offers it.
package settings

import (
	"context"
	"time"
)

// Key is a setting's identifier. A closed vocabulary rather than free-form
// strings, so a typo cannot silently create a setting nothing reads.
type Key string

const (
	// KeyAdmissionDefault is the admission policy a new meeting starts with.
	// A DEFAULT: the host may change it for their own meeting.
	KeyAdmissionDefault Key = "meeting.admission.default"

	// KeyAdmissionFloor is the most permissive policy a host may choose. A
	// CEILING: a deployment that sets this to "trusted" means no host can
	// open a meeting to anybody holding the link, however much they want to.
	KeyAdmissionFloor Key = "meeting.admission.floor"

	// KeyAttendeeMic and friends are what a participant may do on arrival.
	// DEFAULTS: a host may turn them on or off per meeting.
	KeyAttendeeMic    Key = "meeting.attendee.mic"
	KeyAttendeeCamera Key = "meeting.attendee.camera"
	KeyAttendeeShare  Key = "meeting.attendee.share"
	KeyAttendeeChat   Key = "meeting.attendee.chat"

	// KeyRemoteControl is whether remote control may be requested at all. A
	// CEILING, and the one most worth having: a deployment that does not want
	// anybody driving anybody else's machine turns this off and no consent
	// dialog can ever appear.
	KeyRemoteControl Key = "control.enabled"

	// KeyAllowSignup is whether strangers may create their own accounts.
	// A CEILING.
	KeyAllowSignup Key = "signup.open"

	// KeyOrgDomains lists the email domains that ARE this organisation.
	//
	// Named explicitly rather than inferred from whoever is searching. "Your
	// own domain is your organisation" is the obvious rule and it is wrong in
	// the one case that matters: on a deployment where people sign up with
	// personal addresses it makes every gmail.com account in the world
	// mutually discoverable, which is precisely the exposure the directory
	// scope exists to prevent. An explicit list cannot make that mistake.
	KeyOrgDomains Key = "directory.domains"

	// KeyDirectoryScope decides who a person can find by searching.
	//
	// This is a ceiling and not a preference. On a deployment anybody can
	// reach, a browsable directory is an address book of every account on it,
	// handed to whoever signed up most recently.
	KeyDirectoryScope Key = "directory.scope"

	// KeyRecording is whether meetings may be recorded. A CEILING.
	KeyRecording Key = "meeting.recording.enabled"
)

// Kind says whether a host may move a setting.
type Kind string

const (
	KindDefault Kind = "default"
	KindCeiling Kind = "ceiling"
)

// Spec describes one setting, so the console can render it without hardcoding
// a parallel list that drifts.
type Spec struct {
	Key     Key      `json:"key"`
	Label   string   `json:"label"`
	Help    string   `json:"help"`
	Kind    Kind     `json:"kind"`
	Type    string   `json:"type"` // "bool" or "choice"
	Choices []string `json:"choices,omitempty"`
	Default string   `json:"default"`
}

// Catalogue is every setting this deployment understands, in the order an
// administrator should read them.
//
// Defaults are chosen to be the safe answer rather than the convenient one.
// A deployment that has never opened this console is still one where a leaked
// meeting link does not admit a stranger and nobody can take control of a
// colleague's machine.
var Catalogue = []Spec{
	{
		Key: KeyAdmissionDefault, Label: "Default admission",
		Help:    "What a new meeting starts as. Hosts can change this for their own meetings.",
		Kind:    KindDefault,
		Type:    "choice",
		Choices: []string{"open", "trusted", "knock"},
		Default: "trusted",
	},
	{
		Key: KeyAdmissionFloor, Label: "Most permissive a host may choose",
		Help: "A host cannot make a meeting more open than this, whatever they select.",
		Kind: KindCeiling, Type: "choice",
		Choices: []string{"open", "trusted", "knock"},
		Default: "open",
	},
	{
		Key: KeyAttendeeMic, Label: "Attendees may unmute",
		Help: "Off means attendees join muted and cannot unmute themselves until a host allows it.",
		Kind: KindDefault, Type: "bool", Default: "true",
	},
	{
		Key: KeyAttendeeCamera, Label: "Attendees may turn on video",
		Help: "Off means attendees join with video disabled.",
		Kind: KindDefault, Type: "bool", Default: "true",
	},
	{
		Key: KeyAttendeeShare, Label: "Attendees may share their screen",
		Help: "Off means only hosts can present.",
		Kind: KindDefault, Type: "bool", Default: "true",
	},
	{
		Key: KeyAttendeeChat, Label: "Attendees may use meeting chat",
		Help: "Off means chat is read-only for attendees.",
		Kind: KindDefault, Type: "bool", Default: "true",
	},
	{
		Key: KeyRemoteControl, Label: "Remote control allowed",
		Help: "Off means nobody can request control of another participant's machine, " +
			"and no consent prompt can appear.",
		Kind: KindCeiling, Type: "bool", Default: "true",
	},
	{
		Key: KeyRecording, Label: "Recording allowed",
		Help: "Off means meetings cannot be recorded by anybody.",
		Kind: KindCeiling, Type: "bool", Default: "false",
	},
	{
		Key: KeyOrgDomains, Label: "Your organisation's email domains",
		Help: "Comma separated, for example \"ddecor.com\". People whose address " +
			"is in one of these can find each other by name, the way a company " +
			"address book works. Everyone else is reached by typing their full " +
			"email address. Leave empty on a deployment where people sign up " +
			"with personal addresses -- listing a public provider here would " +
			"publish every account on it to every other.",
		Kind: KindCeiling, Type: "text", Default: "",
	},
	{
		Key: KeyDirectoryScope, Label: "Who people can find by searching",
		Help: "\"organisation\" is the strictest: people find colleagues in the " +
			"domains above and nobody else, so an outside address reaches " +
			"nothing -- the way a locked-down company tenant behaves. " +
			"\"connections\" adds the people you already have a conversation " +
			"with, and lets you reach anybody else by typing their full email " +
			"address. \"everyone\" makes every account browsable by every other, " +
			"which is a shared address book -- right inside one organisation, " +
			"and an email harvesting tool on a deployment strangers can sign " +
			"up to.",
		Kind: KindCeiling, Type: "choice",
		Choices: []string{"organisation", "connections", "everyone"},
		Default: "connections",
	},
	{
		Key: KeyAllowSignup, Label: "Anyone may create an account",
		Help: "Off means only an administrator can add people. " +
			"On means anybody who can reach this server can sign up.",
		Kind: KindCeiling, Type: "bool", Default: "false",
	},
}

// Setting is one stored value.
type Setting struct {
	Key       Key       `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
}

// Repository is the port.
type Repository interface {
	All(ctx context.Context) (map[Key]Setting, error)
	Put(ctx context.Context, s Setting) error
}

// Reader is what other modules depend on: the resolved value of one setting,
// never the whole store.
type Reader interface {
	Value(ctx context.Context, k Key) string
	Bool(ctx context.Context, k Key) bool
}

// specFor finds a setting's description, and reports whether it is one this
// deployment understands at all.
func specFor(k Key) (Spec, bool) {
	for _, s := range Catalogue {
		if s.Key == k {
			return s, true
		}
	}
	return Spec{}, false
}
