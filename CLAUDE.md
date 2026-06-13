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
static/files/upload.js      ← ENIGE echte versie (//go:embed files in static/static.go)
static/files/style.css      ← ENIGE echte versie
static/files/favicon.svg    ← Favicon (F in donker vierkant)
web/static/                 ← MAG NIET BESTAAN — verwijder als ze opduikt
```

Templates zitten in `web/templates/` (geëmbed via `web/embed.go`, `//go:embed templates`).

Als je JS of CSS aanpast: **altijd in `static/files/`**.
Als iemand `web/static/` aanmaakt: `rm -rf web/static/` en opnieuw committen.

---

## Admin panel

- URL: `http://<server-ip>:8081/admin/login`
- Token: staat in `/opt/ferri/.env` onder `ADMIN_TOKEN`
- Extern bereikbaar via poort 8081 (reverse proxy → 8080 intern)
- Diagnosepagina: `http://<server-ip>:8081/admin/diag` (tijdelijk, nog te verwijderen)

---

## Volumes & poorten

- `ferri_ferri_db` — named Docker volume voor SQLite (blijft bij `docker compose down`)
- Poort 8080 intern in container, 8081 extern op de host
- Storage pad in container: `/data/storage` (lokaal of SMB share)

---

## Codebase structuur

```
cmd/server/main.go          # Entrypoint, router setup, dependency wiring
                            # LET OP: scheduler wordt VOOR de router geïnitialiseerd
                            # InitTemplates(loc) aangeroepen na config load (timezone)
internal/
  config/config.go          # Config laden (yaml + env vars)
                            # Server.Timezone: IANA timezone string, default "Europe/Brussels"
  db/db.go                  # SQLite connectie, migrations
  db/migrations/            # SQL migraties (lexicografisch uitgevoerd)
    001_initial.sql
    002_notify_recipients.sql  # ADD COLUMN notify_recipients INTEGER NOT NULL DEFAULT 1
  handler/                  # HTTP handlers
    admin.go                # Admin panel (login, dashboard, settings, storage)
    download.go             # Download pagina + bestand serveren + ZIP
    upload.go               # Upload request pagina + download
    send.go                 # Gecombineerde send/request pagina + transfer aanmaken
                            # renderHomePage() — rendert send.html met mode="send"|"request"
                            # homePageData — gedeeld template data struct
                            # SendPage() — GET /, leest ?mode= query param
                            # Link-only mode: notify_recipients=false, download_url in JSON response
    request.go              # Upload request aanmaken
                            # RequestPage() — roept renderHomePage(..., "request", "") aan
  jobs/scheduler.go         # Background jobs (mail, expiry, cleanup)
                            # RunCleanupNow() = publieke methode voor admin trigger
                            # removeFile() helper — logt WARN bij Remove fouten (SMB zichtbaar)
  mail/
    mailer.go               # SMTP via mail_queue
    wrap.go                 # mail.Wrap(settings, bodyHTML) — gedeelde mail layout
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
                            # Transfer struct heeft NotifyRecipients bool
                            # CreateTransferInput heeft NotifyRecipients bool
                            # GetByID() — selecteert ook notify_recipients
    request.go              # Upload requests CRUD
    mail.go                 # Mail queue
    download.go             # Download events
  tus/handler.go            # TUS upload handler (wraps tusd v2)
                            # enqueueTransferMails() slaat over als !t.NotifyRecipients
  token/token.go            # Token generatie (base58)
static/
  static.go                 # //go:embed files
  files/
    upload.js               # TUS upload client + file drop UI
                            # initSendPage() — ondersteunt link-only mode (showDownloadLink)
                            # delivery toggle: "Notify by email" / "Get a link"
    style.css               # Global CSS (leeg — alle CSS zit inline in base.html)
    favicon.svg             # Favicon
web/
  embed.go                  # //go:embed templates
  templates/
    base.html               # Basis layout (CSS, nav) — alle publieke CSS hier inline
                            # InitTemplates(loc) zet displayLocation voor formatDate
    send.html               # Gecombineerde send + request pagina met tab toggle
                            # Mode-tabs: "Send files" / "Request files"
                            # Delivery-tabs in send panel: "Notify by email" / "Get a link"
                            # history.replaceState() verwijdert ?mode= na tab switch
    download.html           # Ontvanger download pagina
    upload.html             # Upload request pagina (hidden form, auto-submit na upload)
    request.html            # Niet meer in gebruik door handlers (send.html vervangt dit)
    request_created.html    # Bevestiging upload request + copy link knop
    request_download.html   # Requester ziet ontvangen bestanden
    upload_complete.html    # Uploader klaar melding
    password.html           # Wachtwoord prompt
    admin/
      base.html             # Admin layout + vereenvoudigde nav (Overview + Settings)
      dashboard.html        # Gecombineerde overview: stats + transfers + requests + mail
      settings.html         # Instellingen (branding, mail, storage)
      transfers.html        # Niet meer in gebruik (redirect naar /admin)
      mail.html             # Niet meer in gebruik
```

---

## Routes

### Publiek (geen auth)
| Method | Pad | Beschrijving |
|--------|-----|--------------|
| GET | `/health` | Liveness check |
| GET | `/favicon.ico` | Redirect naar /static/favicon.svg |
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
| GET | `/` | Gecombineerde send/request pagina (default: send tab) |
| GET | `/?mode=request` | Gecombineerde pagina, request tab pre-selected |
| POST | `/send` | Transfer aanmaken, returns JSON {transfer_id, download_url?} |
| GET/POST | `/request` | Upload request aanmaken (rendert send.html met mode=request) |

### Admin (IP-restricted + session cookie)
| Method | Pad | Beschrijving |
|--------|-----|--------------|
| GET/POST | `/admin/login` | Login |
| GET | `/admin` | Gecombineerde overview (dashboard + transfers + requests + mail) |
| GET | `/admin/transfers` | Redirect naar /admin |
| POST | `/admin/transfers/{id}/delete` | Transfer hard-delete (bestanden + DB) |
| POST | `/admin/requests/{id}/delete` | Request hard-delete (bestanden + DB) |
| POST | `/admin/cleanup` | Force cleanup job (grace period = 0) |
| GET | `/admin/diag` | Diagnose: pending files per transfer/request (tijdelijk) |
| GET | `/admin/mail` | Mail queue |
| POST | `/admin/mail/{id}/retry` | Mail opnieuw proberen |
| POST | `/admin/mail/{id}/delete` | Mail verwijderen |
| GET | `/admin/settings` | Instellingen pagina |
| POST | `/admin/settings` | Branding/mail instellingen opslaan |
| POST | `/admin/settings/logo` | Logo uploaden |
| POST | `/admin/settings/logo/delete` | Logo verwijderen |
| POST | `/admin/settings/storage` | SMB storage opslaan + activeren |
| POST | `/admin/settings/storage/test` | SMB verbinding testen |
| POST | `/admin/logout` | Uitloggen |

---

## Settings (DB key-value)

| Key | Beschrijving |
|-----|--------------|
| `branding.company_name` | Bedrijfsnaam |
| `branding.logo_url` | Logo URL (type=text, niet type=url!) |
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

**LET OP settings form field names vs DB keys:**
De HTML form gebruikt `branding.welcome_message` maar de DB key is `ui.welcome_message`.
De handler in `AdminSettingsSave` doet de vertaling. Niet aanpassen zonder beide te updaten.
Checkboxes sturen `value="1"` — de handler normaliseert naar `"true"`/`"false"`.

SMB wachtwoord encryptie: `DeriveKey(ADMIN_TOKEN)` → AES-256-GCM.
Als ADMIN_TOKEN verandert, moet het SMB wachtwoord opnieuw ingesteld worden.

---

## Database

- SQLite via `modernc/sqlite` (pure Go, geen CGo)
- WAL mode, foreign keys ON
- Pad in container: `/data/db/app.db` (named volume `ferri_ferri_db`)
- Backup via Litestream naar lokaal bestand: `/data/db/backup`
- Migrations: `internal/db/migrations/` — lexicografisch, automatisch bij startup

### Schema aandachtspunten
- `transfers.notify_recipients` (INTEGER, DEFAULT 1): 0 = link-only mode, geen mails verstuurd
- `transfers.status`: pending → active → expired → deleted
- `upload_requests.status`: open → completed/expired → deleted
- `files.tus_upload_id`: het echte bestandspad op storage (flat UUID), niet `storage_path`

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

### Opslag (KRITIEK)
- TUS uploads worden **flat** opgeslagen: `<storage_root>/<tusd_uuid>` +
  `<storage_root>/<tusd_uuid>.info`. De `transfers/<id>/` en `requests/<id>/`
  submappen worden aangemaakt maar zijn **leeg** — de echte bestanden zijn flat.
- `storage_path` in de DB is een **logisch pad** (`transfers/<id>/<file_id>`),
  niet het echte pad op storage. Het echte pad is `tus_upload_id`.
- Bij elke file verwijdering: altijd BEIDE proberen:
  ```go
  removeFile(mgr, f.StoragePath, id)           // legacy fallback (local)
  removeFile(mgr, f.TUSUploadID.String, id)    // echte locatie op SMB
  ```
- `removeFile()` helper in scheduler.go logt WARN bij fouten — SMB errors zijn nu zichtbaar.

### Cleanup job flow
- **Expiry job** (elke 60 min): zet status op `expired`, enqueues expiry summary mail
- **Cleanup job** (elke 6 uur): verwijdert bestanden van storage voor expired/deleted items
  die de grace period voorbij zijn. Roept daarna `MarkFilesDeleted` + `SoftDelete` aan.
- **Grace period**: configureerbaar via `cleanup_grace_hours` in config.yaml
- **Admin delete**: verwijdert onmiddellijk, geen grace period
- **Force cleanup** (`POST /admin/cleanup`): zelfde als cleanup job maar grace = 0
- `gb_freed` in cleanup logs = berekend uit DB, niet gemeten op storage

### Timezone
- `cfg.Server.Timezone` (default `"Europe/Brussels"`) geladen in `main.go`
- `handler.InitTemplates(loc)` aangeroepen na config load
- `formatDate` template functie gebruikt `t.In(displayLocation).Format(...)`
- Go's embedded IANA database handelt DST automatisch af
- Mail timestamps (expiry summary) ook geconverteerd via `loc` parameter

### Send form — link-only modus
- Toggle "Notify by email" / "Get a link" in de send panel
- Bij "Get a link": `link_only=1` verstuurd, recipients veld verborgen
- Backend: `notify_recipients=false` op transfer, sender als enige ontvanger (voor token)
- TUS completion: `enqueueTransferMails()` slaat alles over als `!t.NotifyRecipients`
- Response bevat `download_url` → JS toont copyable link na upload

### Combined send/request pagina
- `send.html` bevat beide panels: `#panel-send` en `#panel-request`
- Mode-tabs wisselen tussen panels via JS
- `history.replaceState()` verwijdert `?mode=` uit URL na tab switch → refresh = send tab
- `GET /request` rendert dezelfde pagina met mode="request"
- `renderHomePage()` en `homePageData` gedefinieerd in `send.go` — KRITIEK voor compilatie

### Mail layout
- Alle mail HTML gewrapped via `mail.Wrap(settings, bodyHTML)` in `internal/mail/wrap.go`
- Builders in `tus/handler.go`: `buildAvailableHTML`, `buildConfirmHTML`
- Builders in `jobs/scheduler.go`: `buildExpirySummaryHTML`, `buildExpirySummaryText`
- Expiry summary timestamps gebruiken timezone via `loc *time.Location` parameter

### Clipboard copy (HTTP vs HTTPS)
- `navigator.clipboard` vereist HTTPS (secure context)
- Alle copy knoppen gebruiken fallback via `document.execCommand('copy')` voor HTTP
- Patroon: check `navigator.clipboard && window.isSecureContext` eerst

### Andere bekende punten
- `litestream.yml` en `config.yaml` kunnen per ongeluk als **directory** in
  `/opt/ferri/src` terechtkomen (git quirk). Check: `file /opt/ferri/src/litestream.yml`
  Als directory: `rm -rf` en opnieuw kopiëren van `/opt/ferri/`.
- Docker volume heet `ferri_ferri_db` (prefix = naam van de map: `ferri`).
  Bij compose vanuit `/opt/ferri/src` wordt het `src_app_db` — verkeerd volume!
- Container draait als UID 1000 — SMB share en storage map moeten schrijfrechten
  geven aan UID 1000.
- TUS `WARN NetworkTimeoutError` met "feature not supported" tijdens PATCH = harmloze
  tusd-interne warning over niet-ondersteunde optionele SMB extensies, geen echte fout.

---

## Git remote (server)

```bash
# Als git pull faalt met auth error:
git remote set-url origin https://<github-user>:<GITHUB_TOKEN>@github.com/<owner>/Ferri.git
git pull
```

---

## Todo lijst

### Afgewerkt
- [x] Admin overview: gecombineerde pagina + stats + type badges + hard delete
- [x] SMB storage backend + admin UI
- [x] Cleanup job: SMB bugs gefixed, logging verbeterd, MarkFilesDeleted + SoftDelete
- [x] Send + Request pagina's samengevoegd in één tabbed pagina
- [x] UTC display fix: timezone configureerbaar via config, DST automatisch
- [x] Copy link knop op request_created pagina (incl. HTTP fallback)
- [x] Favicon (SVG, /favicon.ico redirect)
- [x] Mail layout refresh: mail.Wrap(), compactere HTML, timezone in timestamps
- [x] Expiry summary mail was niet gewrapped — gefixed
- [x] Compact UI: minder witruimte, past op groot scherm zonder scrollen
- [x] Refresh fix: ?mode= verwijderd uit URL via history.replaceState
- [x] Link-only send modus: "Get a link" toggle, geen mail verstuurd
- [x] Request expiry note toegevoegd op form

### Nog te doen
- [ ] Orphan file cleanup knop in admin (bestanden op SMB die niet in DB staan)
- [ ] Admin diag endpoint verwijderen of beveiligen
