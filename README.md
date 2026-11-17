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
| `playbooks/deploy_tango.yml` | Ansible playbook: copies the binary and deploy files to the server over AWS SSM and runs the deploy. |
| `inventories/nonprod/` | The nonprod EC2, reached with `amazon.aws.aws_ssm` (no SSH). |
| `config/aws-targets.json` | Reviewed target: account, region, instance, SSM transfer bucket, deploy role. |
| `deploy/` | `docker-compose.yml` (Postgres, two game nodes, nginx, certbot) and `scripts/deploy.sh`, which the playbook runs on the server. |
| `iam/create-deploy-role.sh` | One-time: creates the GitHub OIDC deploy role for this repository. |
| `.github/workflows/` | `Deploy Tango backend (SSM)` (one button) and `CI`. |
| `docs/CLIENT_PROTOCOL.md` | How the Unity app talks to the server: RPCs, opcodes, state. |

## Deploy (same model as `dev-sssss/ludo_nakama-ansible`)

- GitHub signs in to AWS with a short-lived **OIDC role**: no AWS keys, no
  SSH keys, no open SSH port.
- **Ansible reaches the EC2 through AWS Systems Manager** Session Manager
  (`amazon.aws.aws_ssm`); files travel through the existing SSM transfer
  bucket `ludo-nonprod-ssm-521503820723-ap-south-1`.
- The server is a **static Go binary** (built for amd64 and arm64) on a
  distroless image, rolled onto two Compose nodes one at a time.

### One-time setup

1. Create the deploy role: open **AWS CloudShell** in the nonprod account
   (521503820723, ap-south-1), upload or paste `iam/create-deploy-role.sh`,
   and run `bash create-deploy-role.sh`. It creates
   `tango-nonprod-infrastructure`, which only this repository's `nonprod`
   GitHub environment can use, limited to SSM sessions on
   `i-060485c20a979c038` and the transfer bucket.
2. EC2 security group: allow inbound **8350** (HTTP) and **9443** (HTTPS);
   80, 443, 7350 and 8443 already belong to the Ludo stack and mail. SSH (22)
   is not needed.
3. DNS (Namecheap, Advanced DNS): **A record** `tango-backend` →
   `52.66.106.80`. The domain is `public_host` in `config/aws-targets.json`.

### Every deploy

Actions → **Deploy Tango backend (SSM)** → **Run workflow**. It tests,
builds, verifies the AWS account/instance/SSM status, runs the playbook over
SSM and checks the server from the internet. The summary shows host and port.

What happens on the server (`/opt/tango-server`):

1. `releases/<sha>/tango-server`: the new binary (the last 5 releases are kept)
2. `docker-compose.yml` + `.env`: release path, ports, and secrets generated
   once on the first deploy (database password, session keys, HTTP key,
   cluster secret, client server key); they never leave the server
3. Postgres starts, the database is migrated, `tango-node1` then
   `tango-node2` are replaced one at a time (live matches move to the other
   node), then nginx
4. Ports: 80/443 if free, otherwise **8350** (HTTP) and the first free of
   8443/9443/… (HTTPS); on the nonprod EC2 that is 8350 and 9443
5. HTTPS: a free Let's Encrypt certificate for `tango-backend.sssss.co.in`.
   On nonprod the host's certbot issues it through `/opt/ludo-acme`, the
   folder the shared Ludo nginx already serves on port 80 for Let's Encrypt,
   so that nginx is not changed. `certbot.timer` renews it; the
   certificate's deploy hook (`bin/sync-cert.sh`) copies it to `certs/` and
   reloads Tango's nginx. Until the A record points at the server the deploy
   stays on HTTP (by IP) and says why; the next run tries again.
6. Timers for certificate renewal and a nightly database backup (`backups/`)
7. Connection details in `connection.txt` (including the server key for the
   Unity client): `sudo cat /opt/tango-server/connection.txt`

It is its own Compose project (`tango-backend`) with its own network, volume
and ports. The Ludo stack, Mailu and everything else on the host are not
touched.

```
cd /opt/tango-server
sudo docker compose ps                  # what is running
sudo docker compose logs -f tango-node1 # server logs
```

```
Unity app ──HTTPS/WSS :9443 (HTTP :8350)──▶ nginx (rate limits, WebSocket)
                            │ least_conn
                 ┌──────────┴──────────┐
            tango-node1           tango-node2     (Go binary on distroless)
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
`tango.admin.delete_account`) need `HTTP_KEY` from `/opt/tango-server/.env`: `POST /v2/rpc/<id>?unwrap=true&http_key=<key>`.
