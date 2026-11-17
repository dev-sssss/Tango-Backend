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

Optional repository variables: `EC2_DEPLOY_PATH` (your Tango folder on the
server; if not set, the workflow uses the one folder named like `*tango*` in
the SSH user's home, `/opt` or `/srv`) and `TANGO_DOMAIN`.

Security group: inbound **22** (SSH from GitHub Actions) and either **80 +
443** (when nothing else on the server uses them: HTTPS) or **7350** (used
automatically when 80/443 already belong to another stack: plain HTTP).

**Every deploy:** Actions → **Deploy to EC2** → **Run workflow**. It tests the
server, builds the image (amd64 + arm64) into GitHub Container Registry,
connects over SSH, and runs `deploy.sh` in your Tango folder, then checks the
server from outside. The run summary shows the host, port and server key for
the Unity client.

What `deploy.sh` does in the Tango folder:

1. installs Docker and the Compose plugin only if missing
2. writes `docker-compose.yml` and `.env` (new image tag, ports, and secrets
   generated once: database password, session keys, HTTP key, cluster
   secret, client server key; they never leave the server)
3. starts Postgres, runs the database migration, then starts or replaces
   `tango-node1` and `tango-node2` one at a time (live matches move to the
   other node), then nginx
4. with ports 80/443: gets a Let's Encrypt certificate for the Elastic IP
   (6-day certificate, renewed automatically) or for `TANGO_DOMAIN`
5. installs timers for certificate renewal and a nightly database backup
   (`backups/`, newest 7 kept)
6. writes the connection details to `connection.txt`

It lives next to other Compose stacks without touching them: its own project
name (`tango-backend`), network and volume; only free ports; only old Tango
images are removed; the registry login for the deploy is kept in a temporary
docker config, so the server's own `docker login` is unchanged.

Manual deploy on the instance (no GitHub): copy `deploy/` there and run
`sudo TANGO_IMAGE=<image> TANGO_HOST=<elastic-ip> deploy/scripts/deploy.sh`.

Useful on the instance:

```
cd <your tango folder>
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
                    PostgreSQL 16  ──nightly──▶ backups/
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
`tango.admin.delete_account`) need `HTTP_KEY` from `.env` in the Tango
folder: `POST /v2/rpc/<id>?unwrap=true&http_key=<key>`.
