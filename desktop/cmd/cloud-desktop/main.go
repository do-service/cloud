// Command cloud-desktop is the desktop client: a menu-bar (tray) icon and
// one window with the connections and the cloud of each, checked items
// synced to the connection's folder (docs/desktop-client.md). Every
// connection's engine runs in this process (internal/app.Manager), under
// the connection's lock, so cloudsync can't sync it at the same time.
//
// For manual checks with a throwaway HOME:
//
//	CLOUD_SIGNIN_NO_BROWSER=1  don't open the browser to sign in: the
//	                           address is logged to stderr instead, for
//	                           another browser (agent-browser) to open
//	CLOUD_TOKEN_STORE=file     keep device tokens in a 0600 file of the
//	                           state directory, not in the keychain, which
//	                           the security tool resolves past $HOME
//	CLOUD_PICK_FOLDER=PATH     the folder dialog answers PATH at once, for
//	                           checks that can't click a native dialog
package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/do-service/cloud/desktop/internal/app"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
)

// dist is filled by `make build`, which copies desktop/frontend's build
// here. Only dist/.gitkeep is committed, as in backend/internal/webui, so
// the package compiles before the first frontend build (the window is
// blank then).
//
//go:embed all:dist
var dist embed.FS

// The tray's strings: Go has no access to the web dictionary, and these
// are the only native ones.
const (
	labelTitle      = "Облако"
	labelOpenWindow = "Открыть окно"
	labelOpenFolder = "Открыть папку"
	labelPause      = "Приостановить синхронизацию"
	labelResume     = "Продолжить синхронизацию"
	labelQuit       = "Выход"
	labelNoConns    = "Нет подключённых облаков"
	labelPickFolder = "Папка для синхронизации"
	labelPickButton = "Выбрать"
)

var statusLabels = map[app.Status]string{
	app.StatusSynced:  "Всё синхронизировано",
	app.StatusSyncing: "Идёт синхронизация…",
	app.StatusPaused:  "Синхронизация на паузе",
	app.StatusOffline: "Нет связи с сервером",
	app.StatusError:   "Есть проблемы с синхронизацией",
}

func main() {
	base, err := state.BaseDir()
	if err != nil {
		log.Fatal(err)
	}
	store := signin.NewStore(base)
	if os.Getenv("CLOUD_TOKEN_STORE") == "file" {
		store = signin.NewFileStore(base)
	}
	openBrowser := signin.OpenBrowser
	if os.Getenv("CLOUD_SIGNIN_NO_BROWSER") != "" {
		// The address carries no secret: the PKCE challenge, not the
		// verifier, and a state that only this sign-in's loopback takes.
		openBrowser = func(u string) error {
			fmt.Fprintln(os.Stderr, "cloud-desktop: sign in at", u)
			return nil
		}
	}
	// The dialog needs the app, made after the Manager (its handler is
	// in the app's options).
	var pickFolder func(start string) (string, error)
	pick := func(start string) (string, error) { return pickFolder(start) }
	if p := os.Getenv("CLOUD_PICK_FOLDER"); p != "" {
		pick = func(string) (string, error) { return p, nil }
	}
	m, err := app.NewManager(app.Options{Base: base, Store: store, OpenBrowser: openBrowser, OpenFolder: openFolder, PickFolder: pick})
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close()

	assets, err := fs.Sub(dist, "dist")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", app.NewProxy(m.Upstream))
	mux.Handle("/app/", m.Handler())
	// Built assets, or the Vite dev server when FRONTEND_DEVSERVER_URL is
	// set (a build without the production tag, see `make dev`).
	mux.Handle("/", application.AssetFileServerFS(assets))

	a := application.New(application.Options{
		Name:        labelTitle,
		Description: "Cloud desktop client",
		Assets:      application.AssetOptions{Handler: mux, DisableLogging: true},
		Mac: application.MacOptions{
			// A menu-bar app: no Dock icon, and closing the window doesn't
			// quit it.
			ActivationPolicy: application.ActivationPolicyAccessory,
		},
	})

	window := a.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "main",
		Title:  labelTitle,
		Width:  1100,
		Height: 720,
		URL:    "/",
	})
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		window.Hide()
		e.Cancel()
	})
	show := func() {
		window.Show()
		window.Focus()
	}
	// From the window's request goroutine: the dialog itself runs on the
	// main thread (PromptForSingleSelection invokes it there and waits).
	pickFolder = func(start string) (string, error) {
		return a.Dialog.OpenFile().
			CanChooseDirectories(true).CanChooseFiles(false).CanCreateDirectories(true).
			SetTitle(labelPickFolder).SetMessage(labelPickFolder).SetButtonText(labelPickButton).
			SetDirectory(start).AttachToWindow(window).
			PromptForSingleSelection()
	}

	tray := a.SystemTray.New()
	tray.SetTemplateIcon(app.TrayIcon(app.StatusSynced))
	tray.SetTooltip(labelTitle)
	t := &trayMenu{a: a, tray: tray, m: m, show: show}
	tray.SetMenu(t.build(nil))

	// Only once the app runs: menus touched from goroutines before that
	// dereference the not yet created platform app (v3.0.0-beta.34).
	a.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) { go t.follow() })

	if err := a.Run(); err != nil {
		log.Fatal(err)
	}
}

// trayMenu keeps the tray's icon and menu in step with the connections.
type trayMenu struct {
	a    *application.App
	tray *application.SystemTray
	m    *app.Manager
	show func()
	// key is what the menu was last built from; the icon's state too.
	key  string
	icon app.Status
}

func (t *trayMenu) follow() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for range tick.C {
		conns := t.m.Connections(t.a.Context())
		st := app.Overall(conns)
		if st != t.icon {
			t.icon = st
			t.tray.SetTemplateIcon(app.TrayIcon(st))
		}
		var key strings.Builder
		key.WriteString(string(st))
		for _, c := range conns {
			fmt.Fprintf(&key, "|%s:%s", c.ID, c.Status)
		}
		if key.String() != t.key {
			t.key = key.String()
			t.tray.SetMenu(t.build(conns))
		}
	}
}

// build makes the menu: the state, open the window, open a folder (one
// per connection when there are several), pause or resume all, quit.
func (t *trayMenu) build(conns []app.ConnInfo) *application.Menu {
	menu := t.a.NewMenu()
	label := labelNoConns
	if len(conns) > 0 {
		label = statusLabels[app.Overall(conns)]
	}
	menu.Add(label).SetEnabled(false)
	menu.AddSeparator()
	menu.Add(labelOpenWindow).OnClick(func(*application.Context) { t.show() })
	switch len(conns) {
	case 0:
	case 1:
		id := conns[0].ID
		menu.Add(labelOpenFolder).OnClick(func(*application.Context) { t.openFolder(id) })
	default:
		sub := menu.AddSubmenu(labelOpenFolder)
		for _, c := range conns {
			sub.Add(c.User + "@" + c.Host).OnClick(func(*application.Context) { t.openFolder(c.ID) })
		}
	}
	if len(conns) > 0 {
		allPaused := !slices.ContainsFunc(conns, func(c app.ConnInfo) bool { return c.Status != app.StatusPaused })
		if allPaused {
			menu.Add(labelResume).OnClick(func(*application.Context) { t.setPaused(false) })
		} else {
			menu.Add(labelPause).OnClick(func(*application.Context) { t.setPaused(true) })
		}
	}
	menu.AddSeparator()
	menu.Add(labelQuit).OnClick(func(*application.Context) { t.a.Quit() })
	return menu
}

func (t *trayMenu) openFolder(id string) {
	if err := t.m.OpenFolder(id); err != nil {
		slog.Warn("cloud-desktop: open the folder", "err", err)
	}
}

func (t *trayMenu) setPaused(p bool) {
	go func() {
		if err := t.m.SetPaused(t.a.Context(), "", p); err != nil {
			slog.Warn("cloud-desktop: pause", "err", err)
		}
	}()
}

// openFolder shows path in Finder (Explorer, the file manager).
func openFolder(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
