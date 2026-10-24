# ssmbak — Secure SMB Backup

Back up your home folder (documents, pictures, projects...) to an SMB share:

- **Encrypted before it leaves your machine** (AES-256-GCM). The share only ever sees ciphertext.
- **Fast:** one streaming pass (read → tar → gzip → encrypt → upload), no temp files, ~20 MiB of RAM no matter how big the backup is.
- **Skips junk:** `node_modules`, `.venv`, `__pycache__`, `site-packages`, `~/go/pkg/mod`, `~/.cargo/registry`, `.cache`, trash and more. See what's skipped with `--dry-run`.
- **Safe:** uploads to a `.part` file and renames when done, only deletes old backups after a *successful* new one, can verify a backup by reading it back, and restore can't write outside the folder you choose.
- Zero dependencies (Go standard library only), single binary, works on Linux, macOS and Windows.

## Quick start

```bash
# 1. Build (needs Go 1.25+)
make build            # or: go install github.com/j0yb0y-m/ssmbak@latest

# 2. Make sure the SMB share is mounted (see "Mounting the share"), then:
./bin/ssmbak init --share /mnt/nas/backups
#    -> writes ~/.config/ssmbak/config.env and prints a strong random password.
#       SAVE THAT PASSWORD in a password manager. Lose it and the backups are unreadable.

# 3. Look before you leap: what gets backed up, what gets skipped?
./bin/ssmbak backup --dry-run        # add -v to list every file

# 4. First real backup, checked by reading it back
./bin/ssmbak backup --verify
```

Only some folders? `ssmbak backup ~/Documents ~/Pictures ~/projects`, or set `SSMBAK_SOURCE` in the config.

## Commands

| Command | What it does |
|---|---|
| `ssmbak init --share PATH` | Creates the config file with a random password (refuses to overwrite) |
| `ssmbak backup [DIR...]` | Backs up your home folder (or the given folders) |
| `ssmbak list` | Lists backups on the share |
| `ssmbak verify [--file NAME]` | Decrypts and unpacks a backup to prove it is intact (default: latest) |
| `ssmbak restore [--file NAME] [--to DIR] [--only PATH]` | Restores into a **new** folder (default `./ssmbak-restore-<time>`) |

Useful `backup` flags: `--dry-run`, `-v`, `--verify`, `--exclude PATTERN` (repeatable), `--compress none|fast|default|best`, `--keep-days N`, `--keep-min N`, `--share PATH`, `--config FILE`. Flags go **before** folder names. `ssmbak <command> -h` shows everything.

Restore examples:

```bash
ssmbak restore --list                                   # what is inside the latest backup?
ssmbak restore --to ~/restored                          # everything
ssmbak restore --to ~/restored --only alice/Documents   # one folder (path as shown by --list)
```

Inside a backup, each source folder is a top-level folder named after it (backing up `/home/alice` gives `alice/Documents/...`).

## What is excluded

Matching is by name at any depth (`node_modules`), or by path relative to the folder being backed up if the pattern contains a `/` (`go/pkg/mod`). `*` and `?` work; `*` doesn't cross `/`.

Built in: `node_modules`, `bower_components`, `.venv`, `venv`, `site-packages`, `__pycache__`, `*.pyc`, `*.egg-info`, `.tox`, `.pytest_cache`, `.mypy_cache`, `.gradle`, `.next`, `.nuxt`, `.terraform`, `.npm`, `.nvm`, `.pyenv`, `.rustup`, `.cargo/registry`, `.cargo/git`, `.m2/repository`, `.nuget/packages`, `go/pkg/mod`, `.cache`, `.local/share/Trash`, Electron app caches under `.config/*/`, `.DS_Store`, `Thumbs.db`, `*.swp`, `*.tmp`, `*~`. The full list is `defaultExcludes` in `exclude.go`.

**Deliberately not excluded** because they are ambiguous: `.git` (your project history!), `target`, `build`, `dist`, `vendor`, `Downloads`. Add what you don't want:

```bash
ssmbak backup --exclude target --exclude '*.iso' --exclude Downloads
# or in the config:  SSMBAK_EXCLUDE=target,*.iso,Downloads
```

The share itself is always skipped, even if it is mounted inside the folder being backed up.

## Configuration

Settings come from (low → high priority): config file, environment variables, flags. The file is plain `KEY=VALUE` lines at `~/.config/ssmbak/config.env` (on Windows `%AppData%\ssmbak\config.env`), or wherever `--config` / `SSMBAK_CONFIG` points. See `.env.example`.

| Setting | Meaning | Default |
|---|---|---|
| `SMB_SHARE_PATH` | Folder on the mounted share for backups | required |
| `BACKUP_PASSWORD` or `SSMBAK_PASSWORD_FILE` | Encryption password, or a file containing it | required |
| `SSMBAK_SOURCE` | Folder(s) to back up, separated by `:` (`;` on Windows) | home folder |
| `SSMBAK_EXCLUDE` | Extra exclude patterns, comma-separated | none |
| `SSMBAK_RETENTION_DAYS` | Delete backups older than this after a good backup; `0` = never | `30` |
| `SSMBAK_KEEP_MIN` | ...but always keep at least this many newest backups | `3` |
| `SSMBAK_COMPRESSION` | `none`, `fast`, `default`, `best` | `fast` |

Unknown settings in the file produce a warning (catches typos like `SSMBAK_SOURC`), and a config file holding the password that other users can read produces a `chmod 600` warning.

## Mounting the share

ssmbak writes to a normal folder, so the SMB share has to be mounted first. That also means the OS handles SMB authentication.

**Linux** (`sudo apt install cifs-utils`):

```bash
sudo mkdir -p /mnt/nas
printf 'username=YOUR_SMB_USER\npassword=YOUR_SMB_PASSWORD\n' | sudo tee /etc/samba/nas-creds >/dev/null
sudo chmod 600 /etc/samba/nas-creds
sudo mount -t cifs //nas.local/backups /mnt/nas \
  -o credentials=/etc/samba/nas-creds,uid=$(id -u),gid=$(id -g),vers=3.1.1,seal
```

`seal` turns on SMB3 encryption on the wire (needs a server that supports it; drop it if the mount fails). To mount at boot, put this in `/etc/fstab`:

```
//nas.local/backups /mnt/nas cifs credentials=/etc/samba/nas-creds,uid=1000,gid=1000,vers=3.1.1,seal,_netdev,nofail,x-systemd.automount 0 0
```

**Windows:** use the UNC path directly, e.g. `SMB_SHARE_PATH=\\nas\backups` (or a mapped drive like `Z:\backups`).

**macOS:** connect in Finder (Go → Connect to Server, `smb://nas.local/backups`) and use `SMB_SHARE_PATH=/Volumes/backups`.

## Running it automatically

Cron (every night at 02:30; log lines are timestamped):

```cron
30 2 * * *  /usr/local/bin/ssmbak backup --verify >> $HOME/.local/state/ssmbak.log 2>&1
```

Or a systemd user timer: `~/.config/systemd/user/ssmbak.service`

```ini
[Unit]
Description=ssmbak backup
[Service]
Type=oneshot
ExecStart=%h/go/bin/ssmbak backup --verify
```

and `~/.config/systemd/user/ssmbak.timer`

```ini
[Unit]
Description=Nightly ssmbak backup
[Timer]
OnCalendar=*-*-* 02:30
Persistent=true
[Install]
WantedBy=timers.target
```

then `systemctl --user enable --now ssmbak.timer`. Exit code is `0` on success, `1` on failure, `2` for usage errors, `130` if interrupted. Files that can't be read (permission denied, deleted mid-backup) are skipped with a warning and don't fail the run.

## Security model

- Everything is encrypted on your machine with AES-256-GCM before upload. The key comes from your password via PBKDF2-HMAC-SHA256 (600,000 iterations) with a fresh random salt for every backup. File names are inside the encrypted archive.
- The data is encrypted in 256 KiB authenticated chunks, so tampering, reordering, and **truncation** (for example an interrupted upload) are all detected on `verify`/`restore`. Nothing unauthenticated is ever unpacked.
- Restore uses Go's `os.Root`, so entries like `../../etc/passwd` or symlink tricks inside an archive can't write outside the destination folder. setuid bits are never restored.
- **The password is the only key.** `ssmbak init` generates 256 random bits for you; keep a copy outside this computer. A password stored in the config file (mode 600) protects against whoever can read the *share*, not against someone who is already logged in as you.
- What the share's owner can still see: that backups exist, when, and how big they are.
- A mounted share is writable by anything running as you, including ransomware. Turn on snapshots on the NAS side if it supports them.
- Retention only ever deletes files named exactly `ssmbak_backup_YYYYMMDD_HHMMSS.tar.gz.enc`, never anything else on the share.

## Backup file format

`ssmbak_backup_<time>.tar.gz.enc` is a gzip-compressed tar stream, encrypted as described at the top of `crypt.go` (8-byte magic `SSMBAK01`, iteration count, chunk size, salt, nonce prefix, then AES-256-GCM chunks). Format details are in that file so you are never locked in to this one binary.

## Development

```bash
make test        # go vet + go test
go test -race ./...   # needs a C compiler
```

The tests cover encryption round trips, tampering and truncation, exclusion rules, retention, a complete backup→restore comparison of a realistic folder tree, and hostile archives trying to escape the restore folder.
