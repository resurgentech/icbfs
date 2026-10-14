// Command icbfs-sync keeps folders on this machine in step with icbfs
// drives: one-way (the folder is the truth; the drive mirrors it, every
// version and delete kept in its history and trash) or two-way.
//
//	icbfs-sync                 on Windows: the tray app (as icbfs-sync tray)
//	icbfs-sync tray            the tray app: syncs, shows status and traffic
//	icbfs-sync run             syncs, logging to the console, until stopped
//	icbfs-sync once [-pair N] [-confirm]   one pass of each pair, then exit
//	icbfs-sync init            write a settings file to start from
//	icbfs-sync set-secret      store the secret key encrypted (Windows)
//	icbfs-sync install         start the tray at login (Windows)
//	icbfs-sync uninstall
//	icbfs-sync paths           where the settings, state and log are
//
// It runs in the user's session, not as a service: a mapped network
// drive is the user's, and exists only while they're logged in.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "" && runtime.GOOS == "windows" {
		cmd = "tray"
	}
	if cmd != "tray" {
		useParentConsole()
	}
	fset := flag.NewFlagSet(cmd, flag.ExitOnError)
	paths := defaultPaths()
	fset.StringVar(&paths.Config, "config", paths.Config, "the settings file")
	fset.StringVar(&paths.Data, "data", paths.Data, "where state, the spool and the log are kept")
	pair := fset.String("pair", "", "once: only this pair")
	confirm := fset.Bool("confirm", false, "once: let held removals go ahead")
	fset.Parse(args)

	var err error
	switch cmd {
	case "tray":
		err = tray(paths)
	case "run":
		err = run(paths)
	case "once":
		err = once(paths, *pair, *confirm)
	case "init":
		err = initConfig(paths)
	case "set-secret":
		err = setSecret(paths)
	case "install":
		err = install()
		if err == nil {
			fmt.Println("icbfs-sync will start when you log in")
		}
	case "uninstall":
		err = uninstall()
	case "paths":
		fmt.Printf("settings: %s\ndata:     %s\nlog:      %s\n", paths.Config, paths.Data, logPath(paths))
	default:
		fmt.Fprintln(os.Stderr, "usage: icbfs-sync [tray|run|once|init|set-secret|install|uninstall|paths] [-config FILE] [-data DIR]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "icbfs-sync:", err)
		os.Exit(1)
	}
}

func logPath(p Paths) string { return filepath.Join(p.Data, "icbfs-sync.log") }

func run(paths Paths) error {
	cfg, err := loadConfig(paths.Config)
	if err != nil {
		return err
	}
	log, err := openLog(logPath(paths), os.Stdout)
	if err != nil {
		return err
	}
	defer log.Close()
	r := newRunner(cfg, paths, log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("icbfs-sync: %d pairs", len(cfg.Pairs))
	r.Start(ctx)
	<-ctx.Done()
	log.Printf("icbfs-sync: stopping")
	r.Stop()
	return nil
}

func once(paths Paths, only string, confirm bool) error {
	cfg, err := loadConfig(paths.Config)
	if err != nil {
		return err
	}
	log, err := openLog(logPath(paths), os.Stdout)
	if err != nil {
		return err
	}
	defer log.Close()
	r := newRunner(cfg, paths, log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failed := false
	for _, p := range r.pairs {
		if only != "" && p.cfg.Name != only {
			continue
		}
		e, err := p.open(ctx)
		if err != nil {
			log.Printf("%s: %v", p.cfg.Name, err)
			failed = true
			continue
		}
		if confirm {
			e.Confirm()
		}
		rep := e.Pass(ctx)
		switch {
		case rep.Refused != "":
			log.Printf("%s: held: %s", p.cfg.Name, rep.Refused)
			failed = true
		default:
			log.Printf("%s: %d of %d done, %d errors, %d renames, %s up, %s down, in %v", p.cfg.Name, rep.Done, rep.Planned,
				len(rep.Errors), rep.Renames, size(rep.BytesUp), size(rep.Down), rep.Finished.Sub(rep.Started).Round(time.Millisecond))
			if len(rep.Errors) > 0 {
				failed = true
			}
		}
		for _, c := range p.closers {
			c()
		}
	}
	if failed {
		return errors.New("not everything synced; see above")
	}
	return nil
}

func initConfig(paths Paths) error {
	if _, err := os.Stat(paths.Config); err == nil {
		return fmt.Errorf("%s exists already", paths.Config)
	}
	if err := saveConfig(paths.Config, template()); err != nil {
		return err
	}
	fmt.Printf("wrote %s: edit it, then icbfs-sync set-secret\n", paths.Config)
	return nil
}

func setSecret(paths Paths) error {
	data, err := os.ReadFile(paths.Config)
	if err != nil {
		return err
	}
	cfg, err := loadConfigRaw(data)
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "secret key: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return err
	}
	secret := strings.TrimSpace(line)
	if secret == "" {
		return errors.New("no secret key given")
	}
	p, err := protect(secret)
	if err != nil {
		return err
	}
	cfg.Store.SecretKey, cfg.Store.SecretKeyProtected = "", p
	if err := saveConfig(paths.Config, cfg); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "stored, encrypted for this Windows account")
	return nil
}
