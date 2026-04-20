# Running the Exit Node

## Local development (gateway on your machine)

No TLS needed locally. Use `ws://` and `--network host` so the container can reach your local gateway.

**1. Start the gateway and API**

```bash
./cmd/gateway/run.sh   # terminal 1
./cmd/api/run.sh       # terminal 2
```

**2. Create a user and token**

```bash
# create a user
curl -s -X POST http://localhost:8081/users \
  -H "Authorization: Bearer admin" \
  -H "Content-Type: application/json" \
  -d '{"display_name": "local-test"}' | jq

# create a token (replace {userID} with the id from above)
curl -s -X POST http://localhost:8081/users/{userID}/tokens \
  -H "Authorization: Bearer admin" \
  -H "Content-Type: application/json" \
  -d '{"label": "my-machine"}' | jq
```

Copy the `token` value — it is shown only once.

**3. Run the exit node container**

```bash
docker run -d \
  --name ambush-exitnode \
  --restart unless-stopped \
  --network host \
  -e AMBUSH_GATEWAY_URL=ws://127.0.0.1:8080 \
  -e AMBUSH_TOKEN=your-token-here \
  ambush-exitnode:test
```

**4. Verify it connected**

```bash
docker logs ambush-exitnode
curl http://localhost:8080/health
# expect: "exitnodes": 1
```

**Useful commands**

```bash
docker logs -f ambush-exitnode   # follow logs
docker stop ambush-exitnode      # stop
docker start ambush-exitnode     # start again
docker rm ambush-exitnode        # remove container
```

---

## Production (gateway on a VPS)

The gateway must be running with TLS. Exit nodes connect over `wss://` and verify the gateway certificate against the CA cert you generated with `gencerts`.

See [docs/tls.md](../../docs/tls.md) for the full cert generation and gateway TLS setup guide. Complete that first.

**1. Generate certs on the gateway machine**

```bash
./cmd/gencerts/run.sh ./certs
```

Copy `ca.crt` — you will need it for every exit node.

**2. Configure the gateway**

In `cmd/gateway/.env`:

```bash
TLS_CERT=./certs/gateway.crt
TLS_KEY=./certs/gateway.key
```

Restart the gateway.

**3. Run the exit node container**

```bash
docker run -d \
  --name ambush-exitnode \
  --restart unless-stopped \
  -e AMBUSH_GATEWAY_URL=wss://your-gateway-ip-or-domain:8080 \
  -e AMBUSH_TOKEN=your-token-here \
  -e AMBUSH_CA_CERT="$(cat /path/to/ca.crt)" \
  ghcr.io/yourname/ambush-exitnode:latest
```

> `AMBUSH_CA_CERT` takes the full PEM content of `ca.crt`. The `$(cat ...)` substitution inlines it automatically. On Windows use `Get-Content ca.crt -Raw` in PowerShell.

**4. Verify it connected**

```bash
curl https://your-gateway-ip-or-domain:8080/health
# expect: "exitnodes": 1
```

---

## Giving instructions to operators

When a user should run an exit node, generate their token via the API and send them this command with their token pre-filled:

```bash
docker run -d \
  --name ambush-exitnode \
  --restart unless-stopped \
  -e AMBUSH_GATEWAY_URL=wss://your-gateway:8080 \
  -e AMBUSH_TOKEN=THEIR_TOKEN_HERE \
  -e AMBUSH_CA_CERT="$(cat ca.crt)" \
  ghcr.io/yourname/ambush-exitnode:latest
```

They paste it, hit enter, done. The container auto-restarts on crash and on reboot.
