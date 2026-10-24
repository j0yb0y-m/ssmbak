// ssmbak backs up your files to an SMB share: encrypted on your machine before
// anything leaves it, streamed in one pass, with junk like node_modules and
// .venv left out.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

var version = "dev"

var errUsage = errors.New("usage error") // the flag package already printed the details

const usageText = `
ssmbak - encrypted, fast backups of your files to an SMB share

Usage:
  ssmbak init --share /mnt/nas/backups   create a config file with a strong random password
  ssmbak backup [flags] [DIR...]         back up your home folder (or the given folders)
  ssmbak list                            list the backups on the share
  ssmbak verify [--file NAME]            check that a backup decrypts and unpacks (default: latest)
  ssmbak restore [flags]                 restore a backup into a new folder
  ssmbak version

Run "ssmbak <command> -h" for the flags of each command.
Tip: "ssmbak backup --dry-run" shows what would be backed up and what is excluded.
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "init":
		err = cmdInit(rest)
	case "backup":
		err = cmdBackup(ctx, rest)
	case "list", "ls":
		err = cmdList(rest)
	case "verify":
		err = cmdVerify(ctx, rest)
	case "restore":
		err = cmdRestore(ctx, rest)
	case "version", "--version", "-v":
		fmt.Println("ssmbak", version)
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "ssmbak: unknown command %q\n\n%s", cmd, usageText)
		return 2
	}

	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, context.Canceled):
		term.logf("interrupted; partial upload removed")
		return 130
	default:
		term.logf("error: %v", err)
		return 1
	}
}

// flag helpers

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ssmbak %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) (set map[string]bool, err error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, errUsage
	}
	set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set, nil
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "", "config file (default "+defaultConfigPath()+")")
}

// commands 

func cmdBackup(ctx context.Context, args []string) error {
	fs := newFlags("backup", "backup [flags] [DIR...]")
	cfgFlag := configFlag(fs)
	share := fs.String("share", "", "folder on the SMB share to write to (overrides SMB_SHARE_PATH)")
	var extra stringList
	fs.Var(&extra, "exclude", "extra exclude pattern, repeatable, e.g. --exclude target --exclude '*.iso'")
	dry := fs.Bool("dry-run", false, "show what would be backed up and what is excluded; write nothing")
	verbose := fs.Bool("v", false, "with --dry-run: also list every file that would be included")
	verify := fs.Bool("verify", false, "after uploading, read the backup back and check it decrypts")
	compress := fs.String("compress", "", "none | fast | default | best (default fast; use none for mostly photos/video)")
	keepDays := fs.Int("keep-days", 30, "delete backups older than this many days after a successful run (0 = never delete)")
	keepMin := fs.Int("keep-min", 3, "...but always keep at least this many of the newest backups")
	set, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	path, must := configPathFor(*cfgFlag)
	cfg, err := loadConfig(path, must)
	if err != nil {
		return err
	}
	o := backupOptions{
		Sources:  cfg.Sources,
		Share:    cfg.Share,
		Password: cfg.Password,
		Excludes: append(cfg.Excludes, extra...),
		KeepDays: cfg.KeepDays,
		KeepMin:  cfg.KeepMin,
		DryRun:   *dry,
		Verbose:  *verbose,
		Verify:   *verify,
	}
	if set["share"] {
		o.Share = expandHome(*share)
	}
	if set["keep-days"] {
		o.KeepDays = *keepDays
	}
	if set["keep-min"] {
		o.KeepMin = *keepMin
	}
	comp := cfg.Compress
	if set["compress"] {
		comp = *compress
	}
	if o.Level, err = parseCompression(comp); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		o.Sources = fs.Args()
	}
	if len(o.Sources) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot find your home folder: %w", err)
		}
		o.Sources = []string{home}
	}
	if o.Share == "" {
		return errors.New("no destination: run `ssmbak init --share /path/to/mounted/share`, or set SMB_SHARE_PATH, or pass --share")
	}
	if !o.DryRun && len(o.Password) > 0 && len(o.Password) < 12 {
		term.logf("warning: your backup password is short; anyone who gets a backup file can try to guess it offline. `ssmbak init` generates a strong one")
	}
	return runBackup(ctx, o)
}

// loadForRead is shared by list, verify and restore.
func loadForRead(cfgFlag, shareFlag string, needPassword bool) (*config, error) {
	path, must := configPathFor(cfgFlag)
	cfg, err := loadConfig(path, must)
	if err != nil {
		return nil, err
	}
	if shareFlag != "" {
		cfg.Share = expandHome(shareFlag)
	}
	if cfg.Share == "" {
		return nil, errors.New("no share folder: set SMB_SHARE_PATH or pass --share")
	}
	if needPassword && cfg.Password == "" {
		return nil, errors.New("no password: set BACKUP_PASSWORD or SSMBAK_PASSWORD_FILE (the same one the backup was made with)")
	}
	return cfg, nil
}

func cmdList(args []string) error {
	fs := newFlags("list", "list [flags]")
	cfgFlag := configFlag(fs)
	share := fs.String("share", "", "folder on the SMB share (overrides SMB_SHARE_PATH)")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadForRead(*cfgFlag, *share, false)
	if err != nil {
		return err
	}
	backups, err := listBackups(cfg.Share)
	if err != nil {
		return fmt.Errorf("cannot read %q: %s", cfg.Share, errText(err))
	}
	if len(backups) == 0 {
		fmt.Println("no ssmbak backups in", cfg.Share)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSIZE\tCREATED\t")
	for i, b := range backups {
		mark := ""
		if i == 0 {
			mark = "(latest)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.Name, humanBytes(b.Size), b.Time.Format("2006-01-02 15:04:05"), mark)
	}
	return tw.Flush()
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := newFlags("verify", "verify [flags]")
	cfgFlag := configFlag(fs)
	share := fs.String("share", "", "folder on the SMB share (overrides SMB_SHARE_PATH)")
	file := fs.String("file", "latest", "backup name or path to check")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadForRead(*cfgFlag, *share, true)
	if err != nil {
		return err
	}
	archive, err := resolveArchive(cfg.Share, *file)
	if err != nil {
		return err
	}
	term.logf("verifying %s ...", filepath.Base(archive))
	start := time.Now()
	vs, err := verifyArchive(ctx, archive, cfg.Password)
	if err != nil {
		return fmt.Errorf("VERIFY FAILED: %w", err)
	}
	term.logf("OK: %d entries, %s of data, decrypted and checked in %s", vs.entries, humanBytes(vs.bytes), humanDuration(time.Since(start)))
	return nil
}

func cmdRestore(ctx context.Context, args []string) error {
	fs := newFlags("restore", "restore [flags]")
	cfgFlag := configFlag(fs)
	share := fs.String("share", "", "folder on the SMB share (overrides SMB_SHARE_PATH)")
	file := fs.String("file", "latest", "backup name or path to restore (see `ssmbak list`)")
	to := fs.String("to", "", "folder to restore into (default ./ssmbak-restore-<backup time>)")
	only := fs.String("only", "", "restore only this path inside the backup, e.g. mahdi/Documents/thesis")
	force := fs.Bool("force", false, "restore into a folder that is not empty, overwriting files")
	list := fs.Bool("list", false, "just list what is inside the backup (honours --only)")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadForRead(*cfgFlag, *share, true)
	if err != nil {
		return err
	}
	archive, err := resolveArchive(cfg.Share, *file)
	if err != nil {
		return err
	}
	dest := *to
	if dest == "" {
		stamp := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(archive), backupPrefix), backupSuffix)
		dest = "ssmbak-restore-" + stamp
	}
	return runRestore(ctx, restoreOptions{
		Archive:  archive,
		Dest:     expandHome(dest),
		Only:     *only,
		Force:    *force,
		List:     *list,
		Password: cfg.Password,
	})
}

func cmdInit(args []string) error {
	fs := newFlags("init", "init --share PATH [flags]")
	cfgFlag := configFlag(fs)
	share := fs.String("share", "", "folder on the mounted SMB share where backups go (required)")
	src := fs.String("source", "", "folder(s) to back up, separated by '"+string(os.PathListSeparator)+"' (default: your home folder)")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if *share == "" {
		return errors.New("--share is required, e.g. ssmbak init --share /mnt/nas/backups")
	}
	path := *cfgFlag
	if path == "" {
		path = defaultConfigPath()
	}
	if path == "" {
		return errors.New("cannot determine a config location; pass --config")
	}

	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return err
	}
	password := base64.RawURLEncoding.EncodeToString(raw)

	var b strings.Builder
	b.WriteString("# ssmbak configuration. Keep this file private (chmod 600).\n")
	fmt.Fprintf(&b, "SMB_SHARE_PATH=%s\n", *share)
	fmt.Fprintf(&b, "BACKUP_PASSWORD=%s\n", password)
	if *src != "" {
		fmt.Fprintf(&b, "SSMBAK_SOURCE=%s\n", *src)
	} else {
		b.WriteString("# SSMBAK_SOURCE=~/Documents:~/Pictures:~/projects   # default: your whole home folder\n")
	}
	b.WriteString("# SSMBAK_EXCLUDE=target,*.iso      # extra patterns on top of the built-in junk list\n")
	b.WriteString("# SSMBAK_RETENTION_DAYS=30         # delete backups older than this (0 = never)\n")
	b.WriteString("# SSMBAK_KEEP_MIN=3                # ...but always keep at least this many\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; edit it, or delete it first if you really want a new password (old backups need the old one)", path)
		}
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	fmt.Printf("Created %s\n\n", path)
	fmt.Printf("Your backup password is:\n\n    %s\n\n", password)
	fmt.Println("SAVE THIS PASSWORD somewhere other than this computer (a password manager).")
	fmt.Println("Backups are encrypted with it; if you lose it and the computer, they cannot be recovered.")
	fmt.Println()
	if fi, err := os.Stat(expandHome(*share)); err != nil || !fi.IsDir() {
		fmt.Printf("Note: %s is not accessible right now. Mount the share before backing up.\n\n", *share)
	}
	fmt.Println("Next:")
	fmt.Println("  ssmbak backup --dry-run    see what would be backed up")
	fmt.Println("  ssmbak backup --verify     make the first backup and check it")
	return nil
}
