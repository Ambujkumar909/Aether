# Running Aether on an Ubuntu server

One container, one data volume. These steps take a fresh Ubuntu 24.04 server
with a public IPv4 address to a running deployment with a real certificate.

## 1. The server

- 2 vCPU and 4 GB RAM is a comfortable start; media is forwarded, never
  transcoded, so CPU grows with participants rather than with resolution.
- A **public IPv4 address**. Meetings send media straight to it on UDP.
- A DNS **A record** for your hostname (e.g. `meet.example.com`) pointing at
  that address. The certificate is requested for this name on first start.

## 2. Install Docker

```sh
sudo apt-get update && sudo apt-get install -y ca-certificates curl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" | sudo tee /etc/apt/sources.list.d/docker.list
sudo apt-get update && sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
```

## 3. Open exactly these ports

| Port | Protocol | What uses it |
|---|---|---|
| 443 | TCP | the web app, the API, the chat and meeting sockets (HTTPS) |
| 80 | TCP | certificate challenges, and a redirect to HTTPS |
| 8801 | UDP and TCP | all meeting audio, video and screen sharing |
| 3478 | UDP and TCP | the TURN relay, for networks that block direct UDP |
| 49160-49359 | UDP | relay allocations (one per relayed participant) |

```sh
sudo ufw allow 22/tcp
sudo ufw allow 80/tcp && sudo ufw allow 443/tcp
sudo ufw allow 8801 && sudo ufw allow 3478
sudo ufw allow 49160:49359/udp
sudo ufw enable
```

A cloud provider's own firewall (security group) needs the same rules.

## 4. Configure and start

```sh
git clone https://github.com/Ambujkumar909/Aether.git && cd Aether
cp deploy/env.example deploy/aether.env
nano deploy/aether.env      # public IP, hostname, ACME email, first admin
docker compose up -d --build
docker compose logs -f aether
```

On the first start the log prints the bootstrap administrator's password
(once) and the certificate is obtained within a minute. Open
`https://meet.example.com`, sign in as the administrator, and change the
password.

## 5. What the container is allowed to do

It runs as an unprivileged user with every Linux capability dropped, a
read-only filesystem (only `/data` is writable), no privilege escalation, a
process limit and rotated logs. The image has no shell or package manager.
All state is in the `aether-data` volume: the database, uploaded files, the
ACME account and certificates.

## 6. Upgrading

```sh
git pull && docker compose up -d --build
```

The database migrates itself on start. Take a backup first (below).

## Backups

The server snapshots itself every 6 hours into `/data/backups` and keeps the
newest 28 (a week); uploaded files are mirrored beside them. A snapshot is a
complete SQLite database, checked before it is kept, taken without pausing
the service. Tune with `AETHER_BACKUP_EVERY`, `AETHER_BACKUP_KEEP`,
`AETHER_BACKUP_DIR`.

`/data/aether.key` is the key that seals stored secrets (two-step sign-in).
It is copied into `/data/backups` with every snapshot; a database restored on
a new server without it has every second-step secret unreadable, so restore
the key to `/data/aether.key` alongside the database.

Take one now, and copy the backups off the server (a backup on the same disk
does not survive the disk):

```sh
docker compose exec aether /app/aether -backup-now
docker run --rm -v aether_aether-data:/data -v "$PWD":/out busybox \
  tar czf /out/aether-backups.tgz -C /data backups
```

Restore (the service must be stopped; the replaced database is kept as
`aether.db.before-restore`):

```sh
docker compose stop aether
docker compose run --rm aether -restore /data/backups/aether-<time>.db
docker compose start aether
```

## Email

Mail does two things: password reset, and proving an address at sign-up --
with mail configured, open sign-up creates an account only once the emailed
link is opened with the password chosen (a stranger cannot register your
colleagues' addresses). Without mail, sign-up stays immediate, except that
addresses in the organisation's own domains must be created by an
administrator. Point
`AETHER_SMTP_ADDR` (and user, password, from) at any SMTP server in
`deploy/aether.env`. The connection is always encrypted (TLS on 465, STARTTLS
required elsewhere) and the server's certificate is verified; for a mail
server with a private CA, mount its certificate and set `AETHER_SMTP_CA`.
Links point at `AETHER_PUBLIC_URL` (default: https:// and the first ACME
domain), never at whatever host a request named. Without mail, the sign-in
page does not offer reset.

A link works once, for 30 minutes; asking again replaces it, and one account
is sent at most one a minute. Using it ends every session the account had.

## Single sign-on

Set `AETHER_SSO_ISSUER`, `AETHER_SSO_CLIENT_ID` and `AETHER_SSO_CLIENT_SECRET`
(see `env.example`) and the sign-in page shows "Sign in with <AETHER_SSO_NAME>".
Register Aether at your provider with the redirect URL
`https://<your host>/api/identity/sso/callback`. Only addresses the provider
marks verified, in `AETHER_SSO_DOMAINS` (or the organisation's domains
setting), may sign in this way; a new address becomes a member account.
People signed in by the provider are not asked for the local second step.

## Two-step sign-in

Anyone can turn on two-step sign-in in the console's account card: a code
from an authenticator app (any TOTP app) after the password, and ten
single-use recovery codes shown once. For a lost phone with no recovery
codes, an administrator uses **Remove two-step** on the account in the
console's user list; that ends every session the account has, and the person
sets it up again after signing in with the password. Secrets are stored
sealed with /data/aether.key (see Backups).

## Windows app

The image carries the Windows app and serves it at
`https://your-server/desktop/aether-desktop.exe`. Installing it is running
it once with `-install`:

```
aether-desktop.exe -install -server https://your-server
```

It copies itself to the user's programs folder (`%LOCALAPPDATA%\Programs\Aether`),
adds a Start menu entry and an entry in Settings > Apps, and starts. Nothing
is elevated and nothing is system-wide; `-uninstall` (or Settings > Apps)
removes all of it, settings included.

Updates come from the server the app is pointed at. Every image build has a
version (`AETHER_VERSION`, or the build time when unset) and publishes it at
`/desktop/latest.json`; the app checks shortly after it starts and every six
hours over the same pinned TLS connection it uses for everything else, downloads
a newer version beside itself, verifies size and SHA-256, and offers **Restart
to update** in its notification-area menu. The swap happens at the restart.
There is no code signing: the trust is the server's TLS identity, which the
whole application already rests on. So `docker compose up -d --build` after a
`git pull` also updates every installed Windows app, at its next check.

## Capacity

Measured against this container on a 24-core machine that also ran the load
(docs/ARCHITECTURE.md, "Capacity, measured on the deployment itself"): one
container holds a 50-person meeting over WebRTC with latency around 10 ms and
nothing stalled, and 100 people in chat and on the meeting socket. 100 people
in one WebRTC meeting does not hold yet (talkers stall, video runs seconds
behind). Provision about 2 GB of memory per 50 people in WebRTC meetings.
Many meetings of up to 50 are the shape this release serves; larger
meetings and more than one server are the next phase.

## Monitoring

Prometheus metrics are served at `http://127.0.0.1:9464/metrics` on the
server itself (published to the host's loopback only; never open 9464 in the
firewall). Aggregates only: meetings, participants, open meeting sockets,
media peers and tracks, bytes on the wire by class, HTTP responses by status
code, database health, the last good backup, Go runtime. No meeting, person
or address appears in any label.

Scrape it from a Prometheus on the same host:

```yaml
scrape_configs:
  - job_name: aether
    static_configs: [{ targets: ["127.0.0.1:9464"] }]
rule_files: [aether-alerts.yml]
```

`deploy/aether-alerts.yml` holds the alert rules: the server down, the
database down, no good backup for 13 hours or a backup failing, more than 2%
server errors, and sustained sign-in rate limiting (someone guessing
passwords). Turn the endpoint off with `AETHER_METRICS_ADDR=` (empty).

## Notes

- **Networks that allow only HTTPS.** The relay's TCP/443 path is off in a
  container, because the web server holds 443; such networks reach the relay
  on TCP 3478 and media on TCP 8801, which most allow.
- **More relay capacity.** Each participant who needs the relay uses one port
  from 49160-49359. Widen the range in `compose.yaml`, `AETHER_RELAY_PORTS`
  and the firewall together.
