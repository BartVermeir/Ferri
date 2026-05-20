# Ferri — Claude context

Lees dit bestand aan het begin van elke sessie. Het bevat alle project-specifieke
kennis die Claude anders telkens opnieuw moet uitzoeken.

---

## Deployment procedure (ALTIJD zo, nooit anders)

```bash
# 1. Bouwen — VANUIT /opt/ferri/src
cd /opt/ferri/src
git pull
docker build -t ferri:latest .

# 2. Opstarten — VANUIT /opt/ferri (niet src!)
cd /opt/ferri
docker compose down && docker compose up -d
docker compose logs --tail=20 -f
```

**NOOIT** `docker compose` uitvoeren vanuit `/opt/ferri/src`.
Die map heeft geen `.env`, geen echte `config.yaml` en geen `litestream.yml`.

---

## Mappenstructuur op de server

```
/opt/ferri/
  .env                  # Secrets: ADMIN_TOKEN, SMTP_PASSWORD
  config.yaml           # Applicatieconfiguratie (zie /opt/ferri/src/config.example.yaml)
  litestream.yml        # Litestream replicatieconfiguratie
  docker-compose.yml    # Docker Compose definitie — altijd vanaf hier runnen
  src/                  # Git repository (broncode)
```

---

## Secrets (.env) — KRITIEK

Bestand: `/opt/ferri/.env`

```
ADMIN_TOKEN=<min 32 tekens, gegenereerd met openssl rand -base64 32>
SMTP_PASSWORD=<smtp2go wachtwoord>
```

**VERPLICHT voor elke `docker compose down && up`:**
```bash
cat /opt/ferri/.env   # controleer of dit niet leeg is!
```

Als leeg: opnieuw aanmaken:
```bash
TOKEN=$(openssl rand -base64 32)
echo "ADMIN_TOKEN=$TOKEN" > /opt/ferri/.env
echo "SMTP_PASSWORD=<wachtwoord>" >> /opt/ferri/.env
cat /opt/ferri/.env   # verifieer
```

---

## Statische bestanden — KRITIEK

Er zijn twee mappen die op `upload.js` lijken. Alleen `static/files/` wordt geëmbed:

```
static/files/upload.js    ← ENIGE echte versie (//go:embed files in static/static.go)
static/files/style.css    ← ENIGE echte versie
web/static/               ← MAG NIET BESTAAN — verwijder als ze opduikt
```

Templates zitten in `web/templates/` (geëmbed via `web/embed.go`, `//go:embed templates`).

Als je JS of CSS aanpast: **altijd in `static/files/`**.
Als iemand `web/static/` aanmaakt: `rm -rf web/static/` en opnieuw committen.

---

## Admin panel

- URL: `http://172.16.10.136:8081/admin/login`
- Token: staat in `/opt/ferri/.env` onder `ADMIN_TOKEN`
- Extern bereikbaar via poort 8081 (reverse proxy → 8080 intern)

---

## Volumes & poorten

- `ferri_ferri_db` — named Docker volume voor SQLite (blijft bij `docker compose down`)
- Poort 8080 intern in container, 8081 extern op de host
- Storage pad in container: `/data/storage` (lokaal of SMB share)

---

## Codebase structuur

```
cmd/server/main.go          # Entrypoint, router setup, dependency wiring
internal/
  config/config.go          # Config laden (yaml + env vars)
  db/db.go                  # SQLite connectie, migrations
  db/migrations/            # SQL migraties (lexicografisch uitgevoerd)
  handler/                  # HTTP handlers
    admin.go                # Admin panel (login, dashboard, settings, storage)
    download.go             # Download pagina + bestand serveren
    upload.go               # Upload request pagina + download
    send.go                 # Send pagina (nieuwe transfer aanmaken)
    request.go              # Upload request aanmaken
  jobs/scheduler.go         # Background jobs (mail, expiry, cleanup)
  mail/mailer.go            # SMTP via mail_queue
  middleware/               # Auth, IP allowlist, settings inject, recovery
  storage/                  # Storage backends
    backend.go              # Backend interface + Manager (hot-swap)
    local.go                # Lokaal filesystem
    smb.go                  # SMB/CIFS via go-smb2
    tus_smb.go              # Tusd DataStore implementatie voor SMB
    encrypt.go              # AES-256-GCM voor SMB wachtwoord
  store/                    # Database access layer
    settings.go             # Runtime settings (key-value in DB)
    transfer.go             # Transfers CRUD
    request.go              # Upload requests CRUD
    mail.go                 # Mail queue
    download.go             # Download events
  tus/handler.go            # TUS upload handler (wraps tusd v2)
  token/token.go            # Token generatie (base58)
static/
  static.go                 # //go:embed files
  files/
    upload.js               # TUS upload client + file drop UI
    style.css               # Global CSS
web/
  embed.go                  # //go:embed templates
  templates/
    base.html               # Basis layout (CSS, nav)
    send.html               # Bestanden versturen
    download.html           # Ontvanger download pagina
    upload.html             # Upload request pagina (externe uploader)
    request.html            # Upload request aanmaken
    request_created.html    # Bevestiging upload request
    request_download.html   # Requester ziet ontvangen bestanden
    upload_complete.html    # Uploader klaar melding
    password.html           # Wachtwoord prompt
    admin/                  # Admin templates
```

---

## Routes

### Publiek (geen auth)
| Method | Pad | Beschrijving |
|--------|-----|--------------|
| GET | `/health` | Liveness check |
| GET/POST | `/dl/{token}` | Download pagina + wachtwoord |
| GET | `/dl/{token}/file/{fileID}` | Bestand downloaden |
| GET | `/dl/{token}/zip` | Alle bestanden als ZIP |
| GET/POST | `/ul/{token}` | Upload request pagina + wachtwoord |
| POST | `/ul/{token}/complete` | Uploader klaar signaal |
| GET | `/ul/{token}/files` | Requester ziet ontvangen bestanden |
| GET | `/ul/{token}/file/{fileID}` | Bestand downloaden (requester) |
| GET | `/ul/{token}/zip` | ZIP (requester) |
| POST | `/tus/*` | TUS upload chunks |

### IP-restricted (intern netwerk: 172.16.0.0/12, 10.0.0.0/8, 192.168.0.0/16)
| Method | Pad | Beschrijving |
|--------|-----|--------------|
| GET | `/` | Send pagina |
| POST | `/send` | Transfer aanmaken |
| GET/POST | `/request` | Upload request aanmaken |

### Admin (IP-restricted + session cookie)
| Method | Pad | Beschrijving |
|--------|-----|--------------|
| GET/POST | `/admin/login` | Login |
| GET | `/admin` | Dashboard |
| GET | `/admin/transfers` | Alle transfers |
| POST | `/admin/transfers/{id}/delete` | Transfer verwijderen |
| GET | `/admin/mail` | Mail queue |
| GET | `/admin/settings` | Instellingen pagina |
| POST | `/admin/settings` | Branding/mail instellingen opslaan |
| POST | `/admin/settings/logo` | Logo uploaden |
| POST | `/admin/settings/storage` | SMB storage opslaan + activeren |
| POST | `/admin/settings/storage/test` | SMB verbinding testen |
| POST | `/admin/logout` | Uitloggen |

---

## Settings (DB key-value)

| Key | Beschrijving |
|-----|--------------|
| `branding.company_name` | Bedrijfsnaam |
| `branding.logo_url` | Logo URL |
| `branding.primary_color` | Primaire kleur (hex) |
| `branding.accent_color` | Accent kleur (hex) |
| `branding.bg_color` | Achtergrond kleur (hex) |
| `branding.font_family` | Lettertype |
| `ui.welcome_message` | Welkomstbericht op send pagina |
| `ui.send_page_title` | Titel send pagina |
| `ui.download_page_title` | Titel download pagina |
| `mail.from_name` | Afzendernaam in mails |
| `mail.from_address` | Afzender e-mailadres |
| `mail.notify_on_download` | Mail bij download (true/false) |
| `mail.expiry_summary` | Vervalmail (true/false) |
| `storage.type` | `local` of `smb` |
| `storage.smb_host` | SMB hostname/IP |
| `storage.smb_share` | Share naam |
| `storage.smb_base_path` | Pad binnen de share |
| `storage.smb_username` | Gebruikersnaam |
| `storage.smb_password_encrypted` | AES-256-GCM encrypted wachtwoord |
| `storage.smb_domain` | Domein (optioneel) |

SMB wachtwoord encryptie: `DeriveKey(ADMIN_TOKEN)` → AES-256-GCM.
Als ADMIN_TOKEN verandert, moet het SMB wachtwoord opnieuw ingesteld worden.

---

## Database

- SQLite via `modernc/sqlite` (pure Go, geen CGo)
- WAL mode, foreign keys ON
- Pad in container: `/data/db/app.db` (named volume `ferri_ferri_db`)
- Backup via Litestream naar lokaal bestand: `/data/db/backup`
- Migrations: `internal/db/migrations/` — lexicografisch, automatisch bij startup

---

## Stack & dependencies

- Go 1.23
- `github.com/go-chi/chi/v5` — router
- `github.com/tus/tusd/v2` — TUS upload protocol
- `github.com/hirochachacha/go-smb2` — SMB2/CIFS client
- `modernc.org/sqlite` — SQLite (pure Go)
- `golang.org/x/crypto` — bcrypt voor wachtwoorden
- Litestream 0.3.13 — SQLite replicatie
- Distroless runtime image (geen shell in container)

---

## Bekende aandachtspunten

- `litestream.yml` en `config.yaml` kunnen per ongeluk als **directory** in
  `/opt/ferri/src` terechtkomen (git quirk). Check: `file /opt/ferri/src/litestream.yml`
  Als directory: `rm -rf` en opnieuw kopiëren van `/opt/ferri/`.
- Docker volume heet `ferri_ferri_db` (prefix = naam van de map: `ferri`).
  Bij compose vanuit `/opt/ferri/src` wordt het `src_app_db` — verkeerd volume!
- Container draait als UID 1000 — SMB share en storage map moeten schrijfrechten
  geven aan UID 1000.
- TUS uploads worden flat opgeslagen: `<storage_root>/<tusd_uuid>` +
  `<storage_root>/<tusd_uuid>.info`. De `transfers/<id>/` submappen worden
  aangemaakt maar zijn leeg — de echte bestanden zijn flat.
- `findFileInStorage` in `upload.go` is een legacy fallback voor oude uploads
  zonder `tus_upload_id` in de DB. Alleen actief voor local storage.

---

## Git remote (server)

```bash
# Als git pull faalt met auth error:
git remote set-url origin https://BartVermeir:GITHUB_TOKEN@github.com/BartVermeir/Ferri.git
git pull
```

---

## Todo lijst (stand van zaken)

- [ ] Admin transfers: type kolom, totale grootte, klikbare links
- [ ] Admin: upload requests aparte sectie
- [ ] Copy link knop op request_created pagina
- [ ] favicon.ico 404
- [ ] Mail layout finetuning
- [x] SMB storage backend
- [x] Storage UI in admin settings
