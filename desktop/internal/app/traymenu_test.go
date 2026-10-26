package app

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/update"
)

// labels lists the menu as text: "---" a separator, "(x)" a disabled
// item, "x[a, b]" a submenu.
func labels(items []TrayItem) []string {
	var out []string
	for _, it := range items {
		switch {
		case it.Separator:
			out = append(out, "---")
		case it.Sub != nil:
			out = append(out, it.Label+"["+strings.Join(labels(it.Sub), ", ")+"]")
		case it.Disabled || it.Action == TrayNone:
			out = append(out, "("+it.Label+")")
		default:
			out = append(out, it.Label)
		}
	}
	return out
}

func TestTrayMenu(t *testing.T) {
	upToDate := update.Status{Mode: update.ModeAuto, Enabled: true, Install: update.InstallSelf, State: update.StateIdle}
	one := []ConnInfo{{ID: "c1", User: "ivan", Host: "cloud.example", Status: StatusSynced}}
	two := append(slices.Clone(one), ConnInfo{ID: "c2", User: "petr", Host: "other.example", Status: StatusPaused})
	paused := []ConnInfo{{ID: "c1", User: "ivan", Host: "cloud.example", Status: StatusPaused}}
	found := upToDate
	found.State, found.Release = update.StateAvailable, &update.Release{Version: "1.2.0"}
	ready := found
	ready.State = update.StateReady
	manual := found
	manual.Install = update.InstallManual
	off := upToDate
	off.Mode = update.ModeOff
	dev := upToDate
	dev.Enabled = false
	checking := upToDate
	checking.State = update.StateChecking
	latest := upToDate
	latest.State = update.StateUpToDate
	failed := upToDate
	failed.State, failed.ErrorStage = update.StateError, update.StageCheck

	tail := []string{"---", "Настройки…", "Проверить обновления", "---", "Выход"}
	for _, tc := range []struct {
		name    string
		conns   []ConnInfo
		us      update.Status
		checked bool
		want    []string
	}{
		{"nothing", nil, upToDate, false, append([]string{"(Нет подключённых облаков)", "---", "Открыть окно"}, tail...)},
		{"one", one, upToDate, false, append([]string{"(Всё синхронизировано)", "---", "Открыть окно", "Открыть папку", "Приостановить синхронизацию"}, tail...)},
		{"two", two, upToDate, false, append([]string{"(Всё синхронизировано)", "---", "Открыть окно", "Открыть папку[ivan@cloud.example, petr@other.example]", "Приостановить синхронизацию"}, tail...)},
		{"paused", paused, upToDate, false, append([]string{"(Синхронизация на паузе)", "---", "Открыть окно", "Открыть папку", "Продолжить синхронизацию"}, tail...)},
		{"found", nil, found, false, append([]string{"(Нет подключённых облаков)", "Доступна версия 1.2.0 — установить…", "---", "Открыть окно"}, tail...)},
		{"ready", nil, ready, false, append([]string{"(Нет подключённых облаков)", "Версия 1.2.0 загружена — установить…", "---", "Открыть окно"}, tail...)},
		{"package install", nil, manual, false, append([]string{"(Нет подключённых облаков)", "Доступна версия 1.2.0", "---", "Открыть окно"}, tail...)},
		// «Не проверять» stops the schedule, not the user's own check.
		{"updates off", nil, off, false, append([]string{"(Нет подключённых облаков)", "---", "Открыть окно"}, tail...)},
		{"development build", nil, dev, false, []string{"(Нет подключённых облаков)", "---", "Открыть окно", "---", "Настройки…", "(Обновления: сборка для разработки)", "---", "Выход"}},
		{"checking", nil, checking, true, []string{"(Нет подключённых облаков)", "---", "Открыть окно", "---", "Настройки…", "(Проверяем обновления…)", "---", "Выход"}},
		{"checked: latest", nil, latest, true, []string{"(Нет подключённых облаков)", "---", "Открыть окно", "---", "Настройки…", "Проверить обновления", "(Установлена последняя версия)", "---", "Выход"}},
		{"latest, not asked", nil, latest, false, append([]string{"(Нет подключённых облаков)", "---", "Открыть окно"}, tail...)},
		{"checked: failed", nil, failed, true, []string{"(Нет подключённых облаков)", "---", "Открыть окно", "---", "Настройки…", "Проверить обновления", "(Не удалось проверить обновления)", "---", "Выход"}},
		{"checked: found", nil, found, true, append([]string{"(Нет подключённых облаков)", "Доступна версия 1.2.0 — установить…", "---", "Открыть окно"}, tail...)},
	} {
		items := TrayMenu(tc.conns, tc.us, tc.checked)
		if got := labels(items); !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}

	// What the items do.
	items := TrayMenu(two, found, false)
	actions := map[string]TrayItem{}
	for _, it := range items {
		actions[it.Label] = it
	}
	for label, want := range map[string]TrayAction{
		"Доступна версия 1.2.0 — установить…": TrayShowUpdate,
		"Открыть окно":                TrayOpenWindow,
		"Приостановить синхронизацию": TrayPause,
		"Настройки…":                  TraySettings,
		"Проверить обновления":        TrayCheckUpdates,
		"Выход": TrayQuit,
	} {
		if actions[label].Action != want {
			t.Errorf("%s: %q", label, actions[label].Action)
		}
	}
	if sub := actions["Открыть папку"].Sub; len(sub) != 2 || sub[1].Action != TrayOpenFolder || sub[1].ConnID != "c2" {
		t.Errorf("folders: %+v", sub)
	}
	if TrayKey(TrayMenu(one, upToDate, false)) == TrayKey(TrayMenu(one, found, false)) {
		t.Error("the menu's key misses the update")
	}
}

// The tray's «Настройки…» reaches the window once, with its next poll of
// the connections.
func TestRequestView(t *testing.T) {
	base := t.TempDir()
	m, err := NewManager(Options{Base: base, Store: signin.NewFileStore(base)})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	h := m.Handler()
	var list struct {
		View *string `json:"view"`
	}
	if call(t, h, http.MethodGet, "/app/v1/connections", nil, &list); list.View != nil {
		t.Fatalf("a view nobody asked for: %q", *list.View)
	}
	m.RequestView(ViewSettings)
	if call(t, h, http.MethodGet, "/app/v1/connections", nil, &list); list.View == nil || *list.View != "settings" {
		t.Fatalf("view: %v", list.View)
	}
	list.View = nil
	if call(t, h, http.MethodGet, "/app/v1/connections", nil, &list); list.View != nil {
		t.Fatalf("the view twice: %q", *list.View)
	}
}
