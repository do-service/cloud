package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/connection"
	"github.com/do-service/cloud/desktop/internal/engine"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
)

// setupConnection is connection.Setup with the CLI's wording of its
// refusals.
func setupConnection(ctx context.Context, base string, a signin.Account, dir string) (state.Connection, bool, error) {
	conn, existing, err := connection.Setup(ctx, base, a, dir)
	if errors.Is(err, connection.ErrOtherFolder) {
		return state.Connection{}, false, fmt.Errorf("это облако уже подключено к папке %s", conn.LocalRoot)
	}
	return conn, existing, folderError(err)
}

// folderError words why a folder can't be a sync folder; other errors
// pass as they are.
func folderError(err error) error {
	var what string
	switch {
	case errors.Is(err, state.ErrNested):
		what = "папка оказалась бы внутри папки другого подключения или служебной папки приложения (или содержала бы их)"
	case errors.Is(err, connection.ErrNotFolder):
		what = "это не папка"
	case errors.Is(err, connection.ErrFolderNotEmpty):
		what = "папка не пуста: перенести можно только в новую или пустую папку"
	case errors.Is(err, connection.ErrFolderTaken):
		what = "это папка другого подключения (другого облака или аккаунта)"
	case errors.Is(err, connection.ErrInsideCurrent):
		what = "новая папка внутри нынешней"
	case errors.Is(err, connection.ErrCrossVolume):
		what = "папка на другом диске: так переносить нельзя — перенесите файлы сами и подключите облако к папке заново"
	case errors.Is(err, localfs.ErrNoMarker):
		what = "папки синхронизации нет на месте (нет .cloudsync)"
	default:
		return err
	}
	return fmt.Errorf("%s (%w)", what, err)
}

// pickConnection finds a connection by id, server, user name or
// "user@host"; "" is the only one there is.
func pickConnection(base, which string) (state.Connection, error) {
	conns, err := state.OpenRegistry(base).List()
	if err != nil {
		return state.Connection{}, err
	}
	if len(conns) == 0 {
		return state.Connection{}, errors.New("нет подключений: войдите командой cloudsync login <адрес>")
	}
	if which == "" {
		if len(conns) > 1 {
			return state.Connection{}, errors.New("подключений несколько: укажите, какое (cloudsync status покажет все)")
		}
		return conns[0], nil
	}
	norm, _ := api.NormalizeServer(which)
	var found []state.Connection
	for _, c := range conns {
		host := c.Server
		if u, err := url.Parse(c.Server); err == nil {
			host = u.Host
		}
		if c.ID == which || c.Server == norm || c.UserName == which || c.UserName+"@"+host == which {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return state.Connection{}, fmt.Errorf("подключение %q не найдено", which)
	case 1:
		return found[0], nil
	}
	return state.Connection{}, fmt.Errorf("под %q подходят несколько подключений: укажите id", which)
}

// openSession opens a connection for the engine, under its lock: the
// app or another cloudsync with it open makes it fail.
func openSession(ctx context.Context, which string, opts connection.Options) (*connection.Session, error) {
	base, err := state.BaseDir()
	if err != nil {
		return nil, err
	}
	conn, err := pickConnection(base, which)
	if err != nil {
		return nil, err
	}
	opts.Base = base
	s, err := connection.Open(ctx, signin.NewStore(base), conn, opts)
	if errors.Is(err, state.ErrLocked) {
		return nil, fmt.Errorf("%s уже открыто в другом процессе (приложение «Облако» или другой cloudsync): закройте его или управляйте синхронизацией там", connName(conn))
	}
	return s, err
}

func connName(c state.Connection) string {
	host := c.Server
	if u, err := url.Parse(c.Server); err == nil {
		host = u.Host
	}
	return c.UserName + "@" + host
}

// problemText is a problem as the user reads it.
func problemText(p state.Problem) string {
	what := map[string]string{
		engine.ProblemNameInvalid:   "имя не принимает сервер",
		engine.ProblemNameWindows:   "имя недопустимо в Windows, файл не скачан",
		engine.ProblemNameIgnored:   "такие имена клиент не синхронизирует, файл не скачан",
		engine.ProblemCaseCollision: "в папке есть имя, отличающееся только регистром; этот файл не загружен",
		engine.ProblemReadOnly:      "нет прав на запись: изменение осталось только здесь",
		engine.ProblemUnselectKept:  "снята галочка, но несохранённое изменение не удалось загрузить — файл оставлен",
		engine.ProblemConflict:      "конфликтная копия",
		engine.ProblemRescued:       "несохранённое изменение не загрузить туда, где оно лежало (общая папка закрыта или только для чтения): перенесено сюда и загружается в ваше облако",
		engine.ProblemMassDelete:    "удаление приостановлено: подтвердите (cloudsync sync -confirm-deletes)",
		engine.ProblemQuota:         "не хватает места в облаке",
		engine.ProblemLocked:        "файл заблокирован на сервере",
		engine.ProblemForbidden:     "сервер запретил действие",
		engine.ProblemFailing:       "операция повторно не удаётся",
		engine.ProblemNeedsSignIn:   "устройство отключено на сервере: войдите заново (cloudsync login)",
		engine.ProblemNoMarker:      "папка синхронизации недоступна (нет .cloudsync): синхронизация остановлена",
		engine.ProblemOffline:       "нет сети: сервер недоступен, синхронизация продолжится, когда он ответит",
	}[p.Kind]
	if what == "" {
		what = p.Kind
	}
	s := what
	if p.RelPath != "" {
		s = p.RelPath + ": " + what
	}
	if p.Detail != "" && (p.Kind == engine.ProblemMassDelete || p.Kind == engine.ProblemFailing) {
		s += " (" + p.Detail + ")"
	}
	return s
}

var errUsage = errors.New("usage")

func oneArg(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	}
	return "", errUsage
}

func cloudPath(p string) string {
	return "/" + strings.Trim(p, "/")
}
