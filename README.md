# Tango Cards backend

The game server for Tango Cards. All game rules run here, so the Unity app
only shows what the server decides and sends the player's choices back.

Built on the same clustered Go engine as the Ludo backend
(`dev-sssss/ludo_backend_modules`): Nakama-compatible API, PostgreSQL, live
match handover between nodes during deploys, crash recovery from checkpoints,
versioned APIs and abuse limits. The Ludo game code is replaced by the Tango
Cards game.

## What is in this folder

| Path | What it is |
|---|---|
| `server/` | Go server. `internal/game/tango` = rules engine, match handler, bots, payouts. `internal/game/rpc` = every RPC the app calls. `internal/server` = engine (API, sockets, cluster, wallet, friends). `e2e/` = tests that run real server processes. |
| `deploy/` | `docker-compose.yml` (Postgres, two game nodes, nginx, certbot) and `scripts/deploy.sh`, which installs or updates the stack on a server. |
| `.github/workflows/` | `Deploy to EC2` (one button) and `CI`. |
| `docs/CLIENT_PROTOCOL.md` | How the Unity app talks to the server: RPCs, opcodes, state. |

## Deploy to your EC2 instance

**Once:** add three repository secrets (Settings → Secrets and variables →
Actions):

| Secret | Value |
|---|---|
| `EC2_HOST` | the instance's Elastic IP |
| `EC2_USER` | SSH user: `ubuntu` (Ubuntu) or `ec2-user` (Amazon Linux) |
| `EC2_SSH_KEY` | the full contents of the instance's `.pem` private key |

The EC2 security group must allow inbound **22** (SSH from GitHub Actions),
**80** and **443**. Ports 80/443 must not be used by anything else on the
instance.

**Every deploy:** Actions → **Deploy to EC2** → **Run workflow**. It tests the
server, builds the image (amd64 + arm64) into GitHub Container Registry,
copies `deploy/` to the instance over SSH and runs `deploy.sh`, then checks
the server from outside. The run summary shows the host, port and server key
for the Unity client.

What `deploy.sh` does on the instance (`/opt/tango`):

1. installs Docker and the Compose plugin if missing (Ubuntu or Amazon Linux)
2. generates all secrets once into `/opt/tango/.env` (database password,
   session keys, HTTP key, cluster secret, client server key); they never
   leave the server
3. starts Postgres, runs the database migration, then starts or replaces
   `tango-node1` and `tango-node2` one at a time (live matches move to the
   other node), then nginx
4. gets a Let's Encrypt certificate for the Elastic IP (6-day certificate,
   renewed automatically) or for `TANGO_DOMAIN` if that variable is set
5. installs timers for certificate renewal and a nightly database backup
   (`/opt/tango/backups`, newest 7 kept)
6. writes the connection details to `/opt/tango/connection.txt`

Manual deploy on the instance (no GitHub): copy `deploy/` there and run
`sudo TANGO_IMAGE=<image> TANGO_HOST=<elastic-ip> deploy/scripts/deploy.sh`.

Useful on the instance:

```
cd /opt/tango
sudo docker compose ps                  # what is running
sudo docker compose logs -f tango-node1 # server logs
sudo cat connection.txt                 # host, port, server key
```

```
Unity app ──HTTPS/WSS──▶ nginx :443 (TLS, rate limits, WebSocket)
                           │ least_conn
                ┌──────────┴──────────┐
           tango-node1           tango-node2     (Go; API + sockets + matches)
                └── cluster bus ──────┘
                           │
                    PostgreSQL 16  ──nightly──▶ /opt/tango/backups
```

## Game rules implemented on the server

* Classic deck (108) and No Mercy deck (114 with Wild +6 and Wild +10).
* 1v1, 2v2 and 4v4; teammates see each other's cards.
* Stacking: a black +4/+6/+10 goes on any plus; a coloured +2 only on the
  active colour. Nothing to stack → the player draws the stack.
* Challenge: only the first black plus of a stack; guilty player takes the
  stack and the challenger plays, otherwise the challenger takes stack + 2.
* Drawing never adds penalty cards. A drawn card that fits is played
  automatically; a drawn black plus offers Keep / Play.
* TANGO!: the only last-card penalty is an opponent's hammer (+2) on a
  player at one card who did not press it. A wrong hammer costs nothing.
* One round per match. Entry fee is taken at the start; the winner gains the
  fee, others lose by points left in hand, last place loses it all; leaving
  loses it all. Trophies +30 / −10 / −15, 100 trophies per level.
* Public matches fill empty seats with bots after 15 s (normal names, not
  marked as bots). Disconnected players are removed after 3 missed turns.
* Private rooms: 6-digit code, turn time, emotes, spectators, mic setting,
  rematch.

## Working on the server

```
cd server
go test ./...                                             # rules, match, RPC tests
TANGO_TEST_DB=postgres://user:pass@localhost:5432/tango_e2e go test ./...   # + real-process cluster tests
```

Admin RPCs (`tango.admin.grant`, `tango.admin.set_app_config`,
`tango.admin.set_shop_catalog`, `tango.admin.force_finish_match`,
`tango.admin.delete_account`) need `HTTP_KEY` from `/opt/tango/.env` on the
instance: `POST /v2/rpc/<id>?unwrap=true&http_key=<key>`.
