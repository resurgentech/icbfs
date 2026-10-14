//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

// tray runs the pairs and a notification-area icon showing what they're
// doing: one submenu per pair, live traffic, and the commands.
func tray(paths Paths) error {
	name, _ := windows.UTF16PtrFromString(`Local\icbfs-sync`)
	mutex, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return errors.New("icbfs-sync is already running")
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(mutex)

	log, err := openLog(logPath(paths), nil)
	if err != nil {
		return err
	}
	defer log.Close()
	cfg, err := loadConfig(paths.Config)
	if err != nil {
		if os.IsNotExist(err) {
			if err := saveConfig(paths.Config, template()); err == nil {
				exec.Command("notepad.exe", paths.Config).Start()
			}
			return fmt.Errorf("no settings yet: edit %s, then start icbfs-sync again", paths.Config)
		}
		return err
	}
	r := newRunner(cfg, paths, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log.Printf("icbfs-sync tray: %d pairs", len(cfg.Pairs))
	r.Start(ctx)
	systray.Run(func() { trayReady(ctx, r, paths) }, func() {
		cancel()
		r.Stop()
		log.Printf("icbfs-sync tray: stopped")
	})
	return nil
}

type pairMenu struct {
	name                 string
	top                  *systray.MenuItem
	state, last, traffic *systray.MenuItem
	syncNow, pause       *systray.MenuItem
	confirm, open        *systray.MenuItem
	recent               []*systray.MenuItem
}

func trayReady(ctx context.Context, r *Runner, paths Paths) {
	systray.SetIcon(iconIdle)
	systray.SetTooltip("icbfs-sync")
	header := systray.AddMenuItem("icbfs-sync", "")
	header.Disable()
	traffic := systray.AddMenuItem("↑ 0 B/s   ↓ 0 B/s", "data going to and coming from the drive")
	traffic.Disable()
	systray.AddSeparator()

	var menus []*pairMenu
	for _, st := range r.Status() {
		m := &pairMenu{name: st.Name}
		m.top = systray.AddMenuItem(st.Name, st.Folder)
		m.state = m.top.AddSubMenuItem("", "")
		m.state.Disable()
		m.last = m.top.AddSubMenuItem("", "")
		m.last.Disable()
		m.traffic = m.top.AddSubMenuItem("", "")
		m.traffic.Disable()
		m.syncNow = m.top.AddSubMenuItem("Sync now", "start a pass now")
		m.pause = m.top.AddSubMenuItem("Pause", "stop syncing this pair until resumed")
		m.confirm = m.top.AddSubMenuItem("Confirm removals", "let the held removals go ahead")
		m.confirm.Hide()
		m.open = m.top.AddSubMenuItem("Open folder", st.Folder)
		recent := m.top.AddSubMenuItem("Recent", "what the last passes did")
		for i := 0; i < 10; i++ {
			it := recent.AddSubMenuItem("", "")
			it.Disable()
			it.Hide()
			m.recent = append(m.recent, it)
		}
		menus = append(menus, m)
		go pairClicks(ctx, r, m, st.Folder)
	}

	systray.AddSeparator()
	syncAll := systray.AddMenuItem("Sync all now", "")
	openLog := systray.AddMenuItem("Open log", logPath(paths))
	settings := systray.AddMenuItem("Edit settings", paths.Config+" (restart icbfs-sync after editing)")
	restart := systray.AddMenuItem("Restart", "reload the settings")
	quit := systray.AddMenuItem("Quit", "stop syncing")

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-syncAll.ClickedCh:
				r.SyncNow("")
			case <-openLog.ClickedCh:
				exec.Command("notepad.exe", logPath(paths)).Start()
			case <-settings.ClickedCh:
				exec.Command("notepad.exe", paths.Config).Start()
			case <-restart.ClickedCh:
				if exe, err := os.Executable(); err == nil {
					systray.Quit()
					go func() {
						time.Sleep(2 * time.Second) // let this one release its lock
						exec.Command(exe, "tray").Start()
					}()
				}
			case <-quit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()

	// Refresh the menu and the icon every second, with traffic rates.
	go func() {
		var lastUp, lastDown int64
		perPair := map[string][2]int64{}
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			sts := r.Status()
			var up, down int64
			busy, problem, allPaused := false, false, len(sts) > 0
			for i, st := range sts {
				up += st.BytesUp
				down += st.BytesDown
				busy = busy || st.Busy
				problem = problem || st.Last.Refused != "" || len(st.Last.Errors) > 0 || strings.HasPrefix(st.State, "waiting")
				allPaused = allPaused && st.Paused
				if i < len(menus) {
					prev := perPair[st.Name]
					menus[i].show(st, st.BytesUp-prev[0], st.BytesDown-prev[1])
					perPair[st.Name] = [2]int64{st.BytesUp, st.BytesDown}
				}
			}
			traffic.SetTitle(fmt.Sprintf("↑ %s/s   ↓ %s/s", size(up-lastUp), size(down-lastDown)))
			lastUp, lastDown = up, down
			switch {
			case allPaused:
				systray.SetIcon(iconPaused)
				systray.SetTooltip("icbfs-sync: paused")
			case busy:
				systray.SetIcon(iconBusy)
				systray.SetTooltip("icbfs-sync: syncing")
			case problem:
				systray.SetIcon(iconProblem)
				systray.SetTooltip("icbfs-sync: needs attention")
			default:
				systray.SetIcon(iconIdle)
				systray.SetTooltip("icbfs-sync: up to date")
			}
		}
	}()
}

func (m *pairMenu) show(st PairStatus, up, down int64) {
	title := st.Name
	switch {
	case st.Paused:
		title += " — paused"
	case st.Busy:
		title += " — syncing"
	case st.Last.Refused != "":
		title += " — waiting for you"
	case len(st.Last.Errors) > 0:
		title += " — problems"
	}
	m.top.SetTitle(title)
	m.state.SetTitle(trim(st.State, 90))
	if !st.Last.Finished.IsZero() {
		m.last.SetTitle(fmt.Sprintf("Last pass %s: %d done, %d failed, %d renames",
			st.Last.Finished.Local().Format("15:04"), st.Last.Done, len(st.Last.Errors), st.Last.Renames))
	} else {
		m.last.SetTitle("No pass yet")
	}
	m.traffic.SetTitle(fmt.Sprintf("↑ %s/s   ↓ %s/s   (%s up, %s down in all)", size(up), size(down), size(st.BytesUp), size(st.BytesDown)))
	if st.Paused {
		m.pause.SetTitle("Resume")
	} else {
		m.pause.SetTitle("Pause")
	}
	if st.PendingRemoval > 0 {
		m.confirm.SetTitle(fmt.Sprintf("Confirm removing %d files", st.PendingRemoval))
		m.confirm.Show()
	} else {
		m.confirm.Hide()
	}
	for i, it := range m.recent {
		j := len(st.Recent) - 1 - i
		if j >= 0 {
			it.SetTitle(trim(st.Recent[j], 110))
			it.Show()
		} else {
			it.Hide()
		}
	}
}

func pairClicks(ctx context.Context, r *Runner, m *pairMenu, folder string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.syncNow.ClickedCh:
			r.SyncNow(m.name)
		case <-m.pause.ClickedCh:
			for _, st := range r.Status() {
				if st.Name == m.name {
					r.SetPaused(m.name, !st.Paused)
				}
			}
		case <-m.confirm.ClickedCh:
			r.Confirm(m.name)
		case <-m.open.ClickedCh:
			exec.Command("explorer.exe", folder).Start()
		}
	}
}

func trim(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
