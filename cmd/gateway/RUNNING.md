# Running the Gateway

## Local development

No TLS needed locally. Point at your local Supabase or Postgres instance.

**1. Build the image**

```bash
docker build -f cmd/gateway/Dockerfile -t ambush-gateway:local .
```

**2. Run**

```bash
docker run -d \
  --name ambush-gateway \
  --restart unless-stopped \
  -p 8080:8080 \
  -p 1080:1080 \
  -e DATABASE_URL=your-database-url \
  ambush-gateway:local
```

**3. Verify**

```bash
curl http://localhost:8080/health
# expect: {"status":"ok","exitnodes":0,"active_streams":0}
```

---

## Production (VPS)

Generate TLS certs first — see [docs/tls.md](../../docs/tls.md).

**Run with TLS**

Mount the certs directory into the container as a read-only volume:

```bash
docker run -d \
  --name ambush-gateway \
  --restart unless-stopped \
  -p 8080:8080 \
  -p 1080:1080 \
  -v /path/to/certs:/certs:ro \
  -e DATABASE_URL=your-database-url \
  -e TLS_CERT=/certs/gateway.crt \
  -e TLS_KEY=/certs/gateway.key \
  ghcr.io/yourname/ambush-gateway:latest
```

**Verify**

```bash
curl https://your-server:8080/health
```

**Firewall**

Open both ports on your VPS:

```bash
ufw allow 8080   # exit node WebSocket connections
ufw allow 1080   # SOCKS5 clients
```

---

## Useful commands

```bash
docker logs -f ambush-gateway    # follow logs
docker stop ambush-gateway       # stop
docker start ambush-gateway      # start again
docker restart ambush-gateway    # restart (e.g. after cert renewal)
```
