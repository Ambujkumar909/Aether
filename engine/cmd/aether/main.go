// Command aether runs the whole product in one process: identity, audit,
// rooms, the media plane, and the live bit ledger.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	ossignal "os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // zone rules for recurring meetings, inside the image

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/modular"
	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/modules/control"
	"github.com/aether-rtc/aether/internal/modules/identity"
	"github.com/aether-rtc/aether/internal/modules/messaging"
	"github.com/aether-rtc/aether/internal/modules/scheduling"
	"github.com/aether-rtc/aether/internal/modules/settings"
	"github.com/aether-rtc/aether/internal/platform/backup"
	"github.com/aether-rtc/aether/internal/platform/database"
	"github.com/aether-rtc/aether/internal/platform/httpx"
	"github.com/aether-rtc/aether/internal/platform/mail"
	"github.com/aether-rtc/aether/internal/platform/oidc"
	"github.com/aether-rtc/aether/internal/platform/seal"
	"github.com/aether-rtc/aether/internal/relay"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/sfu"
	"github.com/aether-rtc/aether/internal/signal"
	"github.com/pion/webrtc/v4"
)

// version is the release this binary was built as (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	addr := flag.String("addr", ":8443", "listen address")
	webDir := flag.String("web", "../web", "directory served at /")
	dbPath := flag.String("db", "aether.db", "database file")
	// Off by default. Development mode disables the WebSocket origin check,
	// and a flag that has to be remembered in order to be safe will eventually
	// not be. Running locally does not need it: with no origin patterns the
	// check requires Origin to match Host, which it does when the page and the
	// socket come from the same server, as they do here.
	dev := flag.Bool("dev", false, "development mode: permissive origins, debug logging")
	debug := flag.Bool("debug", false, "verbose per-candidate ICE/SDP logging, WITHOUT weakening origin checks (unlike -dev)")
	secure := flag.Bool("secure-cookies", false, "set the Secure attribute on cookies (requires HTTPS)")
	trustProxy := flag.Bool("trust-proxy", false, "trust X-Forwarded-For (only behind a proxy you control)")
	stun := flag.String("stun", "", "STUN server URL")
	enableRelay := flag.Bool("relay", true,
		"run the built-in TURN relay so calls survive networks that block UDP "+
			"(what Zoom/Teams do); listens on UDP/TCP 3478 and TCP 443")
	mediaPort := flag.Int("media-port", 8801,
		"the ONE UDP+TCP port all audio and video uses (0 = random ports, "+
			"which firewalls drop). 8801 is the port Zoom uses, so it is already "+
			"allowed on many networks and recognised by administrators. It cannot "+
			"be the web port: two TCP listeners cannot share one.")
	useTLS := flag.Bool("tls", false,
		"serve HTTPS with a self-signed certificate (required for camera and "+
			"microphone anywhere except localhost)")
	certPath := flag.String("cert", "aether-cert.pem", "TLS certificate path")
	keyPath := flag.String("key", "aether-key.pem", "TLS private key path")
	allowSignup := flag.Bool("allow-signup", true,
		"let visitors create their own accounts (they are always created as members)")
	adminEmail := flag.String("admin-email", "", "bootstrap administrator email")
	adminPass := flag.String("admin-password", "", "bootstrap administrator password (generated if empty)")
	pprofAddr := flag.String("pprof", "",
		"serve Go's profiler on this address, loopback only (e.g. 127.0.0.1:6060); off when empty")
	publicIPFlag := flag.String("public-ip", "",
		"the IPv4 address browsers reach this server on, when it sits behind a 1:1 NAT "+
			"(a container, a cloud VM): media candidates and relay URLs carry it instead "+
			"of the bound address. Empty uses this machine's own address (right on a LAN)")
	relayPorts := flag.String("relay-ports", "",
		"the port range relay allocations use, e.g. 49160-49200; empty is any free port, "+
			"which a container or firewall cannot publish")
	relayAltTCP := flag.Int("relay-alt-tcp", 443,
		"the relay's extra TCP port for networks that allow only HTTPS; 0 turns it off "+
			"(it must be 0 when the web server itself holds 443)")
	acmeDomain := flag.String("acme-domain", "",
		"obtain and renew a certificate for these hostnames (comma-separated) from an ACME "+
			"authority, Let's Encrypt by default; implies HTTPS")
	acmeEmail := flag.String("acme-email", "", "contact address the ACME authority sends expiry notices to")
	acmeCache := flag.String("acme-cache", "", "where ACME keys and certificates persist (default: acme/ beside the database)")
	acmeDirectory := flag.String("acme-directory", "", "ACME directory URL; empty is Let's Encrypt production")
	acmeCA := flag.String("acme-ca", "", "PEM root to trust for -acme-directory (a private or test authority)")
	httpAddr := flag.String("http-addr", "",
		"plain-HTTP listener for ACME challenges and redirects to HTTPS (default :80 with -acme-domain)")
	publicURL := flag.String("public-url", "",
		"the address people reach this server at (https://meet.example.com); links in email point here. Default: https:// and the first -acme-domain")
	smtpAddr := flag.String("smtp-addr", "",
		"outgoing mail server, host:port (465: TLS; any other port must offer STARTTLS); empty: no email, and password reset is off")
	smtpUser := flag.String("smtp-user", "", "outgoing mail user name")
	smtpPassword := flag.String("smtp-password", "", "outgoing mail password (prefer AETHER_SMTP_PASSWORD: a flag shows in the process list)")
	smtpFrom := flag.String("smtp-from", "", "the address email comes from, e.g. noreply@example.com")
	smtpCA := flag.String("smtp-ca", "", "a PEM certificate authority to trust for the mail server, beside the system's")
	signupPerMinute := flag.Int("signup-per-minute", 60,
		"public sign-up: the most sign-ups accepted per minute across every address (0: no ceiling); each one with mail on sends an email")
	signupPerAddress := flag.Int("signup-per-address", 3,
		"public sign-up: the most sign-ups accepted per hour from one address (a lab or office behind one address needs more)")
	signupBlockFile := flag.String("signup-block-domains", "",
		"public sign-up: a file of email domains to refuse silently, one per line (throwaway-mail services)")
	signupCheckMX := flag.Bool("signup-check-mail", true,
		"public sign-up: before sending the verification email, check the address's domain can receive mail (an MX or A record)")
	ssoIssuer := flag.String("sso-issuer", "",
		"single sign-on: the OpenID Connect issuer URL of the organisation's identity provider (empty: off)")
	ssoClientID := flag.String("sso-client-id", "", "single sign-on: the client id registered at the provider")
	ssoClientSecret := flag.String("sso-client-secret", "", "single sign-on: the client secret (prefer AETHER_SSO_CLIENT_SECRET)")
	ssoName := flag.String("sso-name", "your organisation", "single sign-on: the name on the sign-in button")
	ssoDomains := flag.String("sso-domains", "",
		"single sign-on: comma-separated email domains allowed to sign in this way (default: the organisation's domains setting)")
	desktopDir := flag.String("desktop-dir", "",
		"serve the Windows app and its update manifest from this directory at /desktop/ (the image sets it; empty: off)")
	sealKey := flag.String("seal-key", "",
		"file holding the key that seals stored secrets (two-step sign-in); made on first start (default: aether.key beside the database)")
	metricsAddr := flag.String("metrics-addr", "",
		"serve Prometheus metrics at /metrics on this address, plain HTTP: keep it on a private network (e.g. 127.0.0.1:9464); empty turns it off")
	healthcheck := flag.Bool("healthcheck", false,
		"ask the running server on -addr whether it is healthy, exit 0 or 1, and stop (for container health checks)")
	backupDir := flag.String("backup-dir", "", "where database snapshots and the file mirror go (default: backups/ beside the database)")
	backupEvery := flag.Duration("backup-every", 6*time.Hour, "how often the running server snapshots itself; 0 turns scheduled backups off")
	backupKeep := flag.Int("backup-keep", 28, "how many database snapshots to keep (with -backup-every 6h, a week)")
	backupNow := flag.Bool("backup-now", false, "take one backup of -db now (beside a running server is fine), print where, and stop")
	restoreFrom := flag.String("restore", "", "replace -db with this verified snapshot and stop; the server must not be running")
	flag.Parse()
	fromEnv, err := applyEnv(flag.CommandLine, os.LookupEnv)
	if err != nil {
		slog.Error("configuration", "err", err)
		os.Exit(2)
	}
	if *healthcheck {
		os.Exit(checkHealth(*addr, *useTLS || *acmeDomain != ""))
	}
	bopts := backup.Options{DBPath: *dbPath, FilesDir: filepath.Join(filepath.Dir(*dbPath), "files"),
		Dir: *backupDir, Keep: *backupKeep}
	if *sealKey == "" {
		*sealKey = filepath.Join(filepath.Dir(*dbPath), "aether.key")
	}
	bopts.KeyPath = *sealKey
	if bopts.Dir == "" {
		bopts.Dir = filepath.Join(filepath.Dir(*dbPath), "backups")
	}
	if *backupNow {
		res, err := backup.Run(context.Background(), bopts)
		if err != nil {
			slog.Error("backup failed", "err", err)
			os.Exit(1)
		}
		slog.Info("backup taken", "file", res.File, "bytes", res.Bytes, "files_copied", res.FilesCopied, "files_pruned", res.FilesPruned, "removed", res.Removed)
		os.Exit(0)
	}
	if *restoreFrom != "" {
		if err := backup.Restore(context.Background(), *restoreFrom, *dbPath); err != nil {
			slog.Error("restore failed; the database is unchanged", "err", err)
			os.Exit(1)
		}
		slog.Info("restored; the previous database is kept beside it", "from", *restoreFrom, "db", *dbPath, "kept", *dbPath+".before-restore")
		os.Exit(0)
	}
	if *pprofAddr != "" {
		servePprof(*pprofAddr)
	}
	publicIP, err := parsePublicIP(*publicIPFlag)
	if err != nil {
		slog.Error("configuration", "err", err)
		os.Exit(2)
	}
	relayMin, relayMax, err := parsePortRange(*relayPorts)
	if err != nil {
		slog.Error("configuration", "err", err)
		os.Exit(2)
	}
	domains, err := parseDomains(*acmeDomain)
	if err != nil {
		slog.Error("configuration", "err", err)
		os.Exit(2)
	}
	linkBase, err := parsePublicURL(*publicURL, domains)
	if err != nil {
		slog.Error("configuration", "err", err)
		os.Exit(2)
	}
	var smtpCfg *mail.SMTP
	if *smtpAddr != "" {
		if smtpCfg, err = newSMTP(*smtpAddr, *smtpUser, *smtpPassword, *smtpFrom, *smtpCA); err != nil {
			slog.Error("configuration", "err", err)
			os.Exit(2)
		}
	}
	// Serving HTTPS means the session cookie is Secure unless the operator
	// said otherwise: a flag that has to be remembered to be safe eventually
	// is not, and the plain-HTTP dev listener is the only place it is off.
	if (*useTLS || len(domains) > 0) && !wasSet(flag.CommandLine, fromEnv, "secure-cookies") {
		*secure = true
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	if *dev {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	log.Info("aether starting", "version", version, "go", runtime.Version())

	ctx, stop := ossignal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ------------------------------------------------------------------ //
	// Platform
	// ------------------------------------------------------------------ //

	db, err := database.Open(ctx, *dbPath)
	if err != nil {
		log.Error("database unavailable", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	log.Info("database ready", "path", db.Path())

	// One meter at the root. Every byte this process transmits is attributed
	// to a descendant of it, so the number at the top is the sum of what was
	// actually sent rather than a sample.
	root := ledger.NewMeter("node")

	// The SERVER uses no STUN. Only clients do.
	//
	// An SFU knows where it is; it does not need a third party to tell it.
	// Giving it a STUN server anyway made it offer server-reflexive
	// candidates -- its public address as seen from the internet -- from
	// ephemeral ports, and ICE, preferring what connected first, chose them.
	// Two browsers on the SAME LAPTOP were then talking to each other by
	// going out through the college NAT and hairpinning back, which is
	// unreliable, and none of the traffic touched the fixed media port that
	// exists to be firewall-friendly. Meanwhile every browser was ALSO
	// offered only "host" candidates from the server's LAN address, which is
	// what it should always have used.
	//
	// -stun still exists: it is handed to CLIENTS via /api/identity/config
	// so browsers behind NAT can find their own public address. That is the
	// correct and only use of STUN in this system.
	ice := []webrtc.ICEServer(nil)
	_ = stun

	// ------------------------------------------------------------------ //
	// Modules
	//
	// identity and audit reference each other at the wiring level and are
	// strictly one-way at the type level: identity holds an audit.Recorder,
	// audit holds an audit.Guard that identity happens to satisfy. Neither
	// imports the other's internals, which is the module rule that keeps
	// this from becoming a ball of mud.
	// ------------------------------------------------------------------ //

	var identityModule *identity.Module
	guard := guardFunc{
		protect: func(p string, next http.Handler) http.Handler {
			return identityModule.Protect(p, next)
		},
		actor: func(r *http.Request) (string, string) {
			return identityModule.ActorOf(r)
		},
		admin: func(r *http.Request) bool {
			return identityModule.IsAdmin(r)
		},
		name: func(r *http.Request) string {
			return identityModule.NameOf(r)
		},
	}
	auditModule := audit.NewModule(db, guard, log, *trustProxy)

	identityModule = identity.NewModule(db, auditModule.Recorder(), log,
		*secure, *trustProxy, *allowSignup)
	schedulingModule := scheduling.NewModule(db, auditModule.Recorder(), guard,
		log, *trustProxy)

	// nil means same-origin only. "*" disables the check entirely, which is
	// the primary defence against cross-site WebSocket hijacking; SameSite
	// cookies happen to block that too, but a defence that only works because
	// a different defence is also present is not one you get to keep.
	origins := []string(nil)
	if *dev {
		origins = []string{"*"}
		log.Warn("development mode: WebSocket origin checking is DISABLED")
	}
	// Files shared in chats live beside the database, under files/.
	messagingModule := messaging.NewModule(db, identityModule, guard, log, origins,
		filepath.Join(filepath.Dir(*dbPath), "files"))

	// The directory reports who is online, which only the chat hub knows.
	// Wired here rather than by import, so neither module depends on the
	// other's types.
	identityModule.Service.SetPresence(messagingModule.Service)

	// Deployment-wide policy an administrator sets once.
	settingsModule := settings.NewModule(db, auditModule.Recorder(), guard,
		log, *trustProxy)

	// Remote control. Consent lives here; the input codec and injection
	// safety rules live in internal/rc. Note that no administrative
	// permission can start a session -- see internal/rc/doc.go rule 1.
	controlModule := control.NewModule(db, auditModule.Recorder(), guard,
		log, *trustProxy)

	rooms := room.NewModule(root)
	// The media plane binds the same host as the web server. -addr is the
	// operator's statement of what this machine exposes; media honours it.
	bindHost, _, _ := net.SplitHostPort(*addr)
	media, err := sfu.NewModule(root, log, ice, bindHost, *mediaPort)
	if err != nil {
		log.Error("media plane failed to start", "err", err)
		os.Exit(1)
	}
	if publicIP != "" && media.Registry.Transport != nil {
		media.Registry.Transport.PublicIP = publicIP
		log.Info("media: announcing the public address", "public_ip", publicIP, "port", *mediaPort)
	}
	gw := signal.NewGateway(rooms.Registry, media.Registry, root, log)
	if *dev {
		gw.Origin = []string{"*"}
	}
	// The meeting socket needs to know who is behind the connection, because
	// the waiting room lets verified members in and makes everybody else
	// knock. Taken from the guard, which reads the verified session, never
	// from anything the client sends.
	gw.Identify = guard.ActorOf
	gw.TrustProxy = *trustProxy
	// And how each scheduled room admits people, decided by its organiser at
	// scheduling time.
	gw.RoomPolicy = schedulingModule.Service.PolicyForRoom
	// An invitation is the permission to walk in. Whoever the organiser named
	// skips the waiting room; whoever merely holds the link does not.
	gw.Invited = schedulingModule.Service.InvitedToRoom
	gw.CoOrganizer = schedulingModule.Service.CoOrganizerOf
	schedulingModule.Service.QnAPush = gw.PushQnA
	schedulingModule.Service.NotesPush = gw.PushNotes
	gw.OnJoin = schedulingModule.Service.Joined
	gw.OnLeave = schedulingModule.Service.Left
	// A meeting is over when the last person leaves, not when the clock says
	// so, and until then it is live however far past its end time it runs.
	gw.OnEmpty = schedulingModule.Service.RoomEmptied
	// Three or more people in a room are a meeting, and a meeting gets a
	// conversation of its own that outlives it. Named after the scheduled
	// meeting when there is one.
	gw.OnRoster = func(ctx context.Context, room string, people []signal.RosterPerson) {
		mp := make([]messaging.MeetingPerson, 0, len(people))
		for _, p := range people {
			mp = append(mp, messaging.MeetingPerson{UserID: p.UserID, Name: p.Name})
		}
		messagingModule.Service.MeetingJoined(ctx, room, schedulingModule.Service.TitleForRoom(ctx, room), mp)
	}
	schedulingModule.Service.Occupancy = rooms.Registry.Occupancy
	schedulingModule.Service.LiveHost = rooms.Registry.HostSession
	schedulingModule.Service.LiveMember = rooms.Registry.Member
	// The deployment's admission ceiling, enforced where a meeting is written
	// rather than in the interface that offers the choice.
	schedulingModule.Service.ClampAdmission = settingsModule.Service.ClampAdmission
	// Recording and transcription. "Recording allowed" is the deployment's
	// ceiling; every start and stop goes in the audit chain; final lines of
	// speech are kept as the meeting's transcript.
	gw.RecordingAllowed = func(ctx context.Context) bool { return settingsModule.Service.Bool(ctx, settings.KeyRecording) }
	recorder := auditModule.Recorder()
	gw.OnRecording = func(ctx context.Context, userID, room string, recording, on bool) {
		action := audit.ActionRecordingStart
		switch {
		case recording && !on:
			action = audit.ActionRecordingStop
		case !recording && on:
			action = audit.ActionTranscriptStart
		case !recording && !on:
			action = audit.ActionTranscriptStop
		}
		recorder.Record(ctx, audit.Event{ActorID: userID, Action: action, Subject: room, Outcome: audit.OutcomeSuccess})
	}
	gw.OnTranscript = func(ctx context.Context, room string, session time.Time, userID, name, text string, at time.Time) {
		if err := schedulingModule.Service.AppendTranscript(ctx, room, session, userID, name, text, at); err != nil {
			log.Warn("transcript line not kept", "room", room, "err", err)
		}
	}

	// How far a directory search reaches. By default a person can find who
	// they already talk to, and anybody else only by typing a full email
	// address -- so there is no query that discovers an account you had no
	// way to name, and no way to page through the deployment. Both ports are
	// injected rather than imported, so identity depends on neither module.
	identityModule.Service.DirectoryScope = settingsModule.Service.DirectoryScope
	identityModule.Service.Contacts = messagingModule.Service.ContactIDs
	// And the organisation's own domains, so colleagues find each other by
	// name the way a company address book works. Empty by default; naming a
	// public provider here is refused rather than honoured.
	identityModule.Service.OrgDomains = settingsModule.Service.OrgDomains
	// Public sign-up guards (identity/signupguard.go).
	identityModule.Handler.SetSignupCeiling(*signupPerMinute)
	identityModule.Handler.SetSignupPerAddress(*signupPerAddress)
	{
		var blocked map[string]bool
		if *signupBlockFile != "" {
			if blocked, err = identity.LoadBlockedDomains(*signupBlockFile); err != nil {
				log.Error("signup block list", "path", *signupBlockFile, "err", err)
				os.Exit(2)
			}
		}
		g := identity.NewSignupGuard(blocked)
		if !*signupCheckMX {
			g.LookupDomain = nil
		}
		identityModule.Service.Guard = g
		log.Info("sign-up guards", "per_minute", *signupPerMinute, "per_address_hour", *signupPerAddress, "blocked_domains", len(blocked), "mail_check", *signupCheckMX)
	}
	// Single sign-on through the organisation's identity provider.
	if *ssoIssuer != "" {
		if linkBase == "" {
			log.Error("sso: -public-url (or an ACME domain) is required: the provider sends people back to it")
			os.Exit(2)
		}
		prov, err := oidc.New(ctx, *ssoIssuer, *ssoClientID, *ssoClientSecret, linkBase+"/api/identity/sso/callback", nil)
		if err != nil {
			log.Error("sso", "err", err)
			os.Exit(1)
		}
		var ssoDoms []string
		for _, d := range strings.Split(*ssoDomains, ",") {
			if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
				ssoDoms = append(ssoDoms, d)
			}
		}
		identityModule.Handler.SSO = &identity.SSO{Provider: prov, Name: *ssoName, Domains: ssoDoms}
		log.Info("sso: on", "issuer", *ssoIssuer, "name", *ssoName, "domains", ssoDoms, "redirect", linkBase+"/api/identity/sso/callback")
	}
	// The key that seals stored secrets, made on first start and backed up
	// with the database (backup.Options.KeyPath).
	sealKeyBytes, err := seal.LoadOrCreateKey(*sealKey)
	if err != nil {
		log.Error("seal key", "path", *sealKey, "err", err)
		os.Exit(1)
	}
	if identityModule.Service.Sealer, err = seal.New(sealKeyBytes); err != nil {
		log.Error("seal key", "err", err)
		os.Exit(1)
	}
	// Outgoing email, through whatever SMTP server the operator names.
	var mailQueue *mail.Queue
	switch {
	case smtpCfg == nil:
		log.Info("email: off (no -smtp-addr): password reset is not offered")
	case linkBase == "":
		log.Warn("email: -smtp-addr is set but -public-url is not, so links cannot be written: password reset is not offered")
	default:
		mailQueue = mail.NewQueue(smtpCfg, log)
		identityModule.Service.SendReset = resetSender(mailQueue, linkBase)
		// Sign-up proves the address before an account exists (signup.go).
		identityModule.Service.SendVerify = verifySender(mailQueue, linkBase)
		identityModule.Service.SendSignupNotice = noticeSender(mailQueue, linkBase)
		log.Info("email: on", "server", *smtpAddr, "from", *smtpFrom, "links", linkBase)
	}
	// And the other half of that policy: whether a conversation may cross the
	// organisation's boundary at all. Enforced where conversations are made,
	// not only where the directory draws results.
	messagingModule.Service.MayMessage = identityModule.Service.MayMessage
	// Automatic status comes from the rooms alone: in a call while in one,
	// presenting while sharing, and back to what was picked the moment the
	// connection drops. The gateway nudges the loop on every such change so
	// the dot moves at once, not at the next tick.
	messagingModule.Service.LiveNow = rooms.Registry.LiveAccounts
	gw.OnPresence = func(string) { messagingModule.Service.NudgeAutoStatus() }

	k := modular.New()
	if err := k.Register(auditModule, identityModule, settingsModule,
		schedulingModule, messagingModule, controlModule, rooms, media, gw); err != nil {
		log.Error("module registration failed", "err", err)
		os.Exit(1)
	}
	if err := k.Start(ctx); err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	log.Info("modules started", "order", k.Order())
	// Scheduled snapshots, on a connection of their own (see internal/platform/backup).
	if *backupEvery > 0 {
		go func() {
			t := time.NewTicker(*backupEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					res, err := backup.Run(ctx, bopts)
					if err != nil {
						backupFailures.Add(1)
						log.Error("scheduled backup failed", "err", err)
						continue
					}
					backupLastOK.Store(time.Now().Unix())
					log.Info("scheduled backup", "file", res.File, "bytes", res.Bytes, "files_copied", res.FilesCopied, "files_pruned", res.FilesPruned)
				}
			}
		}()
	}
	if *allowSignup {
		log.Warn("self-registration is OPEN: anyone who can reach this server " +
			"can create a member account. Pass -allow-signup=false to close it.")
	}

	if err := bootstrapAdmin(ctx, identityModule.Service, log, *adminEmail, *adminPass); err != nil {
		log.Error("bootstrap failed", "err", err)
		os.Exit(1)
	}

	// ------------------------------------------------------------------ //
	// Routes
	// ------------------------------------------------------------------ //

	mux := http.NewServeMux()
	identityModule.Routes(mux)
	auditModule.Routes(mux)
	// Guess this deployment's own email domain, once, from the founding
	// administrator's address. A company installing this signs in as
	// themselves, and their address says which company it is; making them
	// type it again into a console they may never open leaves the address
	// book quietly not working, which looks like a missing feature rather
	// than a missing setting. Never overwrites a value somebody has set, and
	// refuses a public provider.
	if d := identityModule.Service.SuggestOrgDomain(ctx); d != "" {
		if seeded, err := settingsModule.Service.SeedOrgDomains(ctx, d); err != nil {
			log.Warn("could not record the organisation domain", "err", err)
		} else if seeded {
			log.Info("organisation directory enabled", "domain", d,
				"note", "people at this domain can find each other by name; change it in the console")
		}
	}

	schedulingModule.Routes(mux)
	messagingModule.Routes(mux)

	// The TURN relay, and the client config that points at it. Started here,
	// after the kernel, because it is not a module -- it owns OS listeners
	// (3478, 443) that live for the process, not per-room state. It is the
	// fallback path that makes a call work when the network blocks UDP; the
	// fast UDP path on the media port is unaffected and still preferred.
	var relaySrv *relay.Server
	if *enableRelay {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			log.Error("relay: could not seed credential secret", "err", err)
			os.Exit(1)
		}
		// The address clients actually reach. bindHost when it is a real one;
		// otherwise the default-route interface -- the SAME address the SFU
		// announces -- NOT LANAddresses()[0], which returned the WSL virtual
		// adapter (172.24.64.1) and pointed every relay URL at a subnet that
		// exists only inside this machine.
		relayIP := publicIP
		if relayIP == "" {
			relayIP = bindHost
		}
		if ip := net.ParseIP(relayIP); ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
			relayIP = ""
			if routed := sfu.DefaultAddressPolicy(log).BindAddresses(); len(routed) > 0 {
				relayIP = routed[0]
			} else if lan := LANAddresses(); len(lan) > 0 {
				relayIP = lan[0]
			}
		}
		// The relay forwards to Aether's media server and nowhere else: the
		// addresses the media plane announces or binds, read fresh each time
		// (a laptop's address changes as it moves between networks). Every
		// other peer -- localhost, the private network, a cloud metadata
		// address -- is refused (see internal/relay, Options.Allow).
		mediaOwns := func(ip net.IP) bool {
			own := []string{publicIP, relayIP, bindHost}
			own = append(own, media.Registry.Addrs.BindAddresses()...)
			for _, o := range own {
				if p := net.ParseIP(o); p != nil && !p.IsUnspecified() && p.Equal(ip) {
					return true
				}
			}
			return false
		}
		if relayIP == "" {
			log.Warn("relay: no routable address found; relay disabled")
		} else {
			ports := relay.DefaultPorts
			ports.AltTCP = *relayAltTCP
			if r, err := relay.NewWithOptions(relayIP, base64.StdEncoding.EncodeToString(secret), log, relay.Options{
				Ports: ports, RelayMin: relayMin, RelayMax: relayMax, Allow: mediaOwns,
			}); err != nil {
				log.Warn("relay: not started; calls will need a direct UDP path", "err", err)
			} else {
				relaySrv = r
				defer relaySrv.Close()
			}
		}
	}

	ih := identityModule.Handler
	if relaySrv != nil {
		ih.ICEServers = func() []any {
			servers, err := relaySrv.ICEServers()
			if err != nil {
				log.Warn("relay: could not mint ICE credentials", "err", err)
				return nil
			}
			out := make([]any, len(servers))
			for i, s := range servers {
				out[i] = s
			}
			return out
		}
	}

	// The meeting socket requires an authenticated session. Anonymous
	// meeting access is a product decision, not a default, and defaulting it
	// open is how conferencing products acquire uninvited guests.
	settingsModule.Routes(mux)

	mux.Handle("/rtc", ih.RequireAuth(gw))

	mux.Handle("GET /api/telemetry",
		ih.RequirePermission(identity.PermTelemetryRead,
			http.HandlerFunc(statsHandler(root, rooms.Registry, media.Registry, gw))))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "db", "database unreachable")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	if *desktopDir != "" {
		mux.Handle("GET /desktop/{file}", desktopHandler(*desktopDir))
		log.Info("desktop app served", "dir", *desktopDir)
	}
	mux.Handle("/", noCache(http.FileServer(http.Dir(*webDir))))

	// Gzip innermost, so the access log counts the bytes that went on the
	// wire, not the bytes before compression.
	handler := httpx.Recover(log, httpx.SecurityHeaders(*secure, httpx.Logging(log, httpx.BodyTypes(httpx.Gzip(mux)))))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it kills long-lived WebSocket connections, which
		// is the single most common way this gets misconfigured.
		IdleTimeout: 120 * time.Second,
		// TLS 1.3 only; see tlspolicy.go. Ignored on the plaintext dev
		// listener, which exists so tshark can read the frames.
		TLSConfig: hardenedTLS(),
	}

	var metricsSrv *http.Server
	if *metricsAddr != "" {
		metricsSrv = &http.Server{Addr: *metricsAddr, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
			Handler: metricsHandler(metricsDeps{root: root, rooms: rooms.Registry, media: media.Registry, gw: gw, db: db.DB})}
		go func() {
			log.Info("metrics: Prometheus text format at /metrics", "addr", *metricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics listener failed", "addr", *metricsAddr, "err", err)
			}
		}()
	}

	// A certificate from an ACME authority: the server obtains and renews it,
	// and a plain-HTTP listener answers the challenge and sends every other
	// request to HTTPS.
	var redirectSrv *http.Server
	if len(domains) > 0 {
		cache := *acmeCache
		if cache == "" {
			cache = filepath.Join(filepath.Dir(*dbPath), "acme")
		}
		m, err := newACME(acmeOptions{Domains: domains, Email: *acmeEmail, CacheDir: cache,
			Directory: *acmeDirectory, CAFile: *acmeCA})
		if err != nil {
			log.Error("acme", "err", err)
			os.Exit(1)
		}
		srv.TLSConfig = acmeTLS(m)
		ha := *httpAddr
		if ha == "" {
			ha = ":80"
		}
		redirectSrv = &http.Server{Addr: ha, Handler: httpsRedirect(m, domains),
			ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
		go func() {
			log.Info("acme: plain HTTP for challenges and redirects", "addr", ha)
			if err := redirectSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http listener failed (ACME HTTP-01 challenges will fail)", "addr", ha, "err", err)
			}
		}()
		go func() {
			log.Info("listening", "addr", *addr, "web", *webDir, "scheme", "https", "acme", domains, "cache", cache)
			if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("listen failed", "err", err)
				os.Exit(1)
			}
		}()
	}

	go func() {
		if len(domains) > 0 {
			return // served above
		}
		if !*useTLS {
			log.Info("listening", "addr", *addr, "web", *webDir, "scheme", "http")
			// Said plainly, because the failure it causes looks like a bug in
			// the product rather than a browser rule: over plain HTTP, every
			// browser refuses getUserMedia anywhere except localhost. The page
			// loads, the meeting joins, and there is no camera and no
			// microphone.
			if !isLoopback(*addr) {
				log.Warn("serving plain HTTP on a non-loopback address: " +
					"browsers will refuse camera and microphone access. " +
					"Pass -tls to serve HTTPS with a self-signed certificate.")
			}
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("listen failed", "err", err)
				os.Exit(1)
			}
			return
		}

		hosts, err := ensureCert(*certPath, *keyPath)
		if err != nil {
			log.Error("could not prepare a TLS certificate", "err", err)
			os.Exit(1)
		}
		log.Info("listening", "addr", *addr, "web", *webDir, "scheme", "https",
			"cert", *certPath, "valid_for", hosts)

		port := portOf(*addr)
		for _, ip := range LANAddresses() {
			log.Info("join from this network at", "url",
				"https://"+net.JoinHostPort(ip, port)+"/")
		}
		log.Warn("the certificate is self-signed, so every browser will show a " +
			"warning once. That warning is accurate: nothing has vouched for " +
			"this server except itself. On a network you control it is safe to " +
			"continue past it.")

		if err := srv.ListenAndServeTLS(*certPath, *keyPath); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			log.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if redirectSrv != nil {
		_ = redirectSrv.Shutdown(shutdownCtx)
	}
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	if mailQueue != nil {
		mailQueue.Close()
	}
	if err := k.Stop(shutdownCtx); err != nil {
		log.Error("shutdown errors", "err", err)
	}
}

// guardFunc adapts closures to audit.Guard, so the two modules can be wired
// in either order without one importing the other.
type guardFunc struct {
	protect func(string, http.Handler) http.Handler
	actor   func(*http.Request) (string, string)
	admin   func(*http.Request) bool
	name    func(*http.Request) string
}

func (g guardFunc) Protect(p string, next http.Handler) http.Handler {
	return g.protect(p, next)
}
func (g guardFunc) ActorOf(r *http.Request) (string, string) { return g.actor(r) }
func (g guardFunc) IsAdmin(r *http.Request) bool             { return g.admin(r) }
func (g guardFunc) NameOf(r *http.Request) string            { return g.name(r) }

// bootstrapAdmin creates the first administrator on an empty installation.
//
// A generated password printed once is deliberately chosen over a well-known
// default. Every product that ships admin/admin is compromised by a scanner
// within hours of being exposed, and the operator convenience is not worth
// what it costs.
func bootstrapAdmin(ctx context.Context, svc *identity.Service, log *slog.Logger,
	email, password string) error {

	n, err := svc.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if email == "" {
		email = "admin@aether.local"
	}
	generated := false
	if password == "" {
		password = generatePassword()
		generated = true
	}

	if _, err := svc.CreateUser(ctx, nil, email, "Administrator", password,
		identity.RoleAdmin, "127.0.0.1", "bootstrap"); err != nil {
		return err
	}

	log.Warn("=====================================================")
	log.Warn("  BOOTSTRAP ADMINISTRATOR CREATED")
	log.Warn("  email:    " + email)
	if generated {
		log.Warn("  password: " + password)
		log.Warn("  Shown once. Sign in and change it.")
	} else {
		log.Warn("  password: (as supplied on the command line)")
	}
	log.Warn("=====================================================")
	return nil
}

func generatePassword() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Telemetry
// ---------------------------------------------------------------------------

type statsResponse struct {
	UptimeSeconds float64           `json:"uptimeSeconds"`
	Connections   int               `json:"connections"`
	Rooms         []roomStats       `json:"rooms"`
	Node          classBreakdown    `json:"node"`
	Anatomy       []anatomySegment  `json:"anatomy"`
	Media         []sfu.RouterStats `json:"media"`
	Runtime       runtimeStats      `json:"runtime"`
}

// runtimeStats: what an operator (and the stress harness) watches for a
// leak. OS handles cannot tell one apart from Go keeping threads it made
// during a spike; goroutines can.
type runtimeStats struct {
	Goroutines int    `json:"goroutines"`
	HeapMB     uint64 `json:"heapMB"`
	SysMB      uint64 `json:"sysMB"`
	Threads    int    `json:"threads"`
}

type roomStats struct {
	ID           string  `json:"id"`
	Participants int     `json:"participants"`
	Bytes        int64   `json:"bytes"`
	Efficiency   float64 `json:"efficiency"`
}

type classBreakdown struct {
	Total      int64            `json:"total"`
	Removable  int64            `json:"removable"`
	Efficiency float64          `json:"efficiency"`
	Bitrate    float64          `json:"bitrate"`
	Classes    map[string]int64 `json:"classes"`
}

type anatomySegment struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

func statsHandler(root *ledger.Meter, reg *room.Registry, media *sfu.Registry,
	gw *signal.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := root.Snapshot()

		classes := make(map[string]int64)
		for i, b := range snap.Bytes {
			if b > 0 {
				classes[ledger.Class(i).String()] = b
			}
		}

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		threads, _ := runtime.ThreadCreateProfile(nil)
		resp := statsResponse{
			Runtime: runtimeStats{
				Goroutines: runtime.NumGoroutine(),
				HeapMB:     ms.HeapAlloc >> 20,
				SysMB:      ms.Sys >> 20,
				Threads:    threads,
			},
			UptimeSeconds: snap.Elapsed.Seconds(),
			Connections:   gw.Connections(),
			Node: classBreakdown{
				Total:      snap.Total(),
				Removable:  snap.Removable(),
				Efficiency: snap.Efficiency(),
				Bitrate:    snap.Bitrate(),
				Classes:    classes,
			},
		}

		for _, rm := range reg.All() {
			s := rm.Meter.Snapshot()
			resp.Rooms = append(resp.Rooms, roomStats{
				ID: rm.ID, Participants: rm.Size(),
				Bytes: s.Total(), Efficiency: s.Efficiency(),
			})
		}
		for _, rt := range media.All() {
			resp.Media = append(resp.Media, rt.Stats())
		}

		// The packet anatomy is analytic, served alongside the measurements
		// so the interface can show the theory and the observation together.
		p := ledger.DefaultPath()
		resp.Anatomy = []anatomySegment{
			{"Ethernet + IFG", ledger.EthernetOverhead},
			{"IPv4", ledger.IPv4Header},
			{"UDP", ledger.UDPHeader},
			{"RTP header", ledger.RTPFixedHeader},
			{"RTP extensions", ledger.ExtensionsSize(ledger.StandardExtensions)},
			{"Opus payload", 80},
			{"SRTP tag", p.SRTPTagBytes},
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// noCache keeps the dev loop honest. Nothing is more expensive than debugging
// a bug that was fixed three reloads ago.
// isLoopback reports whether a listen address only accepts local connections.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false // ":8443" listens on every interface
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "8443"
	}
	return port
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no-cache, not no-store: the browser must ask every time, and the
		// file server answers 304 Not Modified from Last-Modified when the
		// file has not changed -- a few hundred bytes instead of the 190 KB
		// script it re-downloaded under no-store on every load. A changed
		// file is still seen on the very next request.
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}
