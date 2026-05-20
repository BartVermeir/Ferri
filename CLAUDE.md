# Ferri — Claude context

Dit bestand wordt gelezen aan het begin van elke Claude sessie zodat basiskennis
over het project niet opnieuw uitgezocht hoeft te worden.

## Deployment procedure (ALTIJD zo, nooit anders)

```bash
# 1. Code pullen en bouwen — VANUIT /opt/ferri/src
cd /opt/ferri/src
git pull
docker build -t ferri:latest .

# 2. Container herstarten — VANUIT /opt/ferri (niet src!)
cd /opt/ferri
docker compose down && docker compose up -d
docker compose logs --tail=20 -f
```

**Nooit** `docker compose` uitvoeren vanuit `/opt/ferri/src` — die map heeft
geen `.env`, geen `config.yaml` en geen `litestream.yml`.

## Mappenstructuur op de server

```
/opt/ferri/
  .env                  # Secrets: ADMIN_TOKEN, SMTP_PASSWORD (NOOIT leeg laten!)
  config.yaml           # Applicatieconfiguratie
  litestream.yml        # Litestream replicatieconfiguratie
  docker-compose.yml    # Docker Compose definitie
  src/                  # Git repository (broncode)
    Dockerfile
    go.mod / go.sum
    cmd/
    internal/
    web/
    ...
```

## Secrets (.env)

Bestand: `/opt/ferri/.env`

```
ADMIN_TOKEN=<min 32 tekens>
SMTP_PASSWORD=<smtp2go wachtwoord>
```

**ALTIJD controleren voor een `docker compose down && up`:**
```bash
cat /opt/ferri/.env
```

Als leeg: opnieuw aanmaken:
```bash
TOKEN=$(openssl rand -base64 32)
echo "ADMIN_TOKEN=$TOKEN" > /opt/ferri/.env
echo "SMTP_PASSWORD=<wachtwoord>" >> /opt/ferri/.env
```

## Admin panel

URL: `http://172.16.10.136:8081/admin/login`  
Token: staat in `/opt/ferri/.env` onder `ADMIN_TOKEN`

## Git remote (server)

```bash
# Als git pull faalt met auth error:
git remote set-url origin https://BartVermeir:GITHUB_TOKEN@github.com/BartVermeir/Ferri.git
```

## Volumes

- `ferri_ferri_db` — SQLite database (named Docker volume, blijft bij `docker compose down`)
- `/data/storage` — bestandsopslag (lokaal of SMB share)

## Statische bestanden — KRITIEK

Er zijn twee mappen die op `upload.js` lijken te wijzen. Alleen `static/files/` wordt geëmbed in de binary:

```
static/files/upload.js   ← DIT is de echte versie (//go:embed files in static/static.go)
web/static/              ← MAG NIET BESTAAN — verwijder deze map als ze opduikt
```

Als je JS of CSS aanpast: **altijd in `static/files/`**, nooit ergens anders.

## Bekende aandachtspunten

- `litestream.yml` en `config.yaml` mogen GEEN directories zijn in `/opt/ferri/src`
  (git kan ze per ongeluk als directory aanmaken). Check met `file /opt/ferri/src/litestream.yml`.
- Docker compose volume heet `ferri_ferri_db` (prefix = map naam `ferri`).
- Poort 8080 intern, 8081 extern via reverse proxy op de host.
- Container draait als UID 1000 — SMB share en storage map moeten schrijfrechten geven aan UID 1000.

## Stack

- Go 1.23, chi router, tusd v2, modernc/sqlite, litestream 0.3.13
- Distroless runtime image (geen shell in container)
- SMB storage via `github.com/hirochachacha/go-smb2`
