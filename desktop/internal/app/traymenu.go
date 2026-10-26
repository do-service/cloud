package app

import (
	"fmt"
	"slices"

	"github.com/do-service/cloud/desktop/internal/update"
)

// The tray's strings: Go has no access to the web dictionary, and these
// are the only native ones.
const (
	LabelTitle         = "Облако"
	labelOpenWindow    = "Открыть окно"
	labelOpenFolder    = "Открыть папку"
	labelPause         = "Приостановить синхронизацию"
	labelResume        = "Продолжить синхронизацию"
	labelSettings      = "Настройки…"
	labelCheckUpdates  = "Проверить обновления"
	labelCheckingNow   = "Проверяем обновления…"
	labelUpToDate      = "Установлена последняя версия"
	labelCheckFailed   = "Не удалось проверить обновления"
	labelUpdateFound   = "Доступна версия %s — установить…"
	labelUpdateReady   = "Версия %s загружена — установить…"
	labelQuit          = "Выход"
	labelNoConns       = "Нет подключённых облаков"
	LabelPickFolder    = "Папка для синхронизации"
	LabelPickButton    = "Выбрать"
	labelDevNoUpdates  = "Обновления: сборка для разработки"
	labelUpdateManualF = "Доступна версия %s"
)

var statusLabels = map[Status]string{
	StatusSynced:  "Всё синхронизировано",
	StatusSyncing: "Идёт синхронизация…",
	StatusPaused:  "Синхронизация на паузе",
	StatusOffline: "Нет связи с сервером",
	StatusError:   "Есть проблемы с синхронизацией",
}

// TrayAction is what clicking a tray item does.
type TrayAction string

const (
	TrayNone         TrayAction = ""
	TrayShowUpdate   TrayAction = "show_update" // the window, whose banner asks before installing
	TrayOpenWindow   TrayAction = "open_window"
	TrayOpenFolder   TrayAction = "open_folder" // ConnID's
	TrayPause        TrayAction = "pause"
	TrayResume       TrayAction = "resume"
	TraySettings     TrayAction = "settings" // the window on «Настройки»
	TrayCheckUpdates TrayAction = "check_updates"
	TrayQuit         TrayAction = "quit"
)

// TrayItem is one item of the tray's menu; a separator has only
// Separator set.
type TrayItem struct {
	Label     string
	Action    TrayAction
	ConnID    string
	Disabled  bool
	Separator bool
	// Sub makes the item a submenu.
	Sub []TrayItem
}

// TrayMenu is the tray's menu: the state, a new version when there is
// one, open the window, open a folder (one per connection when there are
// several), pause or resume all; «Настройки…», «Проверить обновления»
// with the result of a check the tray asked for (checked); quit.
func TrayMenu(conns []ConnInfo, us update.Status, checked bool) []TrayItem {
	label := labelNoConns
	if len(conns) > 0 {
		label = statusLabels[Overall(conns)]
	}
	items := []TrayItem{{Label: label, Disabled: true}}
	if item := UpdateItem(us); item != "" {
		items = append(items, TrayItem{Label: item, Action: TrayShowUpdate})
	}
	items = append(items, TrayItem{Separator: true}, TrayItem{Label: labelOpenWindow, Action: TrayOpenWindow})
	switch len(conns) {
	case 0:
	case 1:
		items = append(items, TrayItem{Label: labelOpenFolder, Action: TrayOpenFolder, ConnID: conns[0].ID})
	default:
		sub := make([]TrayItem, 0, len(conns))
		for _, c := range conns {
			sub = append(sub, TrayItem{Label: c.User + "@" + c.Host, Action: TrayOpenFolder, ConnID: c.ID})
		}
		items = append(items, TrayItem{Label: labelOpenFolder, Sub: sub})
	}
	if len(conns) > 0 {
		if slices.ContainsFunc(conns, func(c ConnInfo) bool { return c.Status != StatusPaused }) {
			items = append(items, TrayItem{Label: labelPause, Action: TrayPause})
		} else {
			items = append(items, TrayItem{Label: labelResume, Action: TrayResume})
		}
	}
	items = append(items, TrayItem{Separator: true}, TrayItem{Label: labelSettings, Action: TraySettings})
	switch {
	case !us.Enabled:
		items = append(items, TrayItem{Label: labelDevNoUpdates, Disabled: true})
	case us.State == update.StateChecking:
		items = append(items, TrayItem{Label: labelCheckingNow, Disabled: true})
	default:
		// Whatever the mode: «Не проверять» stops only the schedule.
		items = append(items, TrayItem{Label: labelCheckUpdates, Action: TrayCheckUpdates})
		if checked {
			switch {
			case us.State == update.StateUpToDate:
				items = append(items, TrayItem{Label: labelUpToDate, Disabled: true})
			case us.State == update.StateError && us.ErrorStage == update.StageCheck:
				items = append(items, TrayItem{Label: labelCheckFailed, Disabled: true})
			}
			// A version found is the item at the top.
		}
	}
	return append(items, TrayItem{Separator: true}, TrayItem{Label: labelQuit, Action: TrayQuit})
}

// UpdateItem is the tray's item for a new version, "" without one.
func UpdateItem(us update.Status) string {
	if us.Release == nil {
		return ""
	}
	if us.Install == update.InstallManual {
		return fmt.Sprintf(labelUpdateManualF, us.Release.Version)
	}
	switch us.State {
	case update.StateReady:
		return fmt.Sprintf(labelUpdateReady, us.Release.Version)
	case update.StateAvailable, update.StateDownloading, update.StateError:
		return fmt.Sprintf(labelUpdateFound, us.Release.Version)
	}
	return ""
}

// TrayKey is what the menu is built from: the menu changes when it does.
func TrayKey(items []TrayItem) string {
	return fmt.Sprint(items)
}
