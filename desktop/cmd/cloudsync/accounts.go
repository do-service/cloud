package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/connection"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
)

func store() (*signin.Store, error) {
	dir, err := state.BaseDir()
	if err != nil {
		return nil, err
	}
	return signin.NewStore(dir), nil
}

// runLogin signs in in the browser and connects the account's sync
// folder. It prints whose account got connected, never the token.
func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	name := fs.String("name", "", "device name in the account's device list (default: this computer's name)")
	dir := fs.String("dir", "", "the sync folder (default ~/Cloud/<server host>)")
	noBrowser := fs.Bool("no-browser", false, "only print the sign-in address (another machine's browser, SSH)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		usage()
	}
	s, err := store()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var open func(string) error
	if *noBrowser {
		open = func(string) error { return nil }
	}
	res, err := signin.Login(ctx, signin.Options{
		Server: fs.Arg(0), DeviceName: *name, OpenBrowser: open,
		OnURL: func(u string) {
			fmt.Fprintf(os.Stderr, "Открываю браузер для входа. Если он не открылся, перейдите по адресу:\n  %s\nЖду (Ctrl+C — отмена)…\n", u)
		},
	})
	if err != nil {
		return err
	}
	a, err := s.Save(res)
	if err != nil {
		return err
	}
	fmt.Printf("Вход выполнен: %s на %s, устройство %q.\n", accountName(a), a.Server, a.DeviceName)
	if a.TokenInFile {
		fmt.Fprintf(os.Stderr, "внимание: системной связки ключей нет, токен — в файле %s (доступен только вам)\n", signin.TokensFile)
	}
	base, err := state.BaseDir()
	if err != nil {
		return err
	}
	conn, existing, err := setupConnection(ctx, base, a, *dir)
	if err != nil {
		return err
	}
	if existing {
		fmt.Printf("Подключение уже было: папка %s.\n", conn.LocalRoot)
	} else {
		fmt.Printf("Папка синхронизации: %s. Пока ничего не отмечено — выберите, что синхронизировать: cloudsync select <путь в облаке>\n", conn.LocalRoot)
	}
	return nil
}

// runMoveFolder moves a connection's sync folder (connection.MoveFolder):
// the app or a cloudsync sync with it open must be closed first.
func runMoveFolder(args []string) error {
	fs := flag.NewFlagSet("move-folder", flag.ExitOnError)
	which := fs.String("c", "", "connection (id, server or user@host); needed when there are several")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		usage()
	}
	base, err := state.BaseDir()
	if err != nil {
		return err
	}
	conn, err := pickConnection(base, *which)
	if err != nil {
		return err
	}
	moved, err := connection.MoveFolder(context.Background(), base, conn, fs.Arg(0))
	if errors.Is(err, state.ErrLocked) {
		return fmt.Errorf("%s открыто в другом процессе (приложение «Облако» или cloudsync sync): закройте его и повторите", connName(conn))
	}
	if err != nil {
		return folderError(err)
	}
	fmt.Printf("Папка синхронизации %s перенесена: %s.\n", connName(conn), moved.LocalRoot)
	return nil
}

func runLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	force := fs.Bool("force", false, "forget the account here even if the server can't be reached to revoke the device")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		usage()
	}
	server, err := api.NormalizeServer(fs.Arg(0))
	if err != nil {
		return err
	}
	s, err := store()
	if err != nil {
		return err
	}
	accounts, err := s.Accounts()
	if err != nil {
		return err
	}
	var match []signin.Account
	for _, a := range accounts {
		if a.Server == server && (fs.Arg(1) == "" || a.Username == fs.Arg(1)) {
			match = append(match, a)
		}
	}
	switch {
	case len(match) == 0:
		return fmt.Errorf("на %s нет вошедшего аккаунта", server)
	case len(match) > 1:
		return fmt.Errorf("на %s несколько аккаунтов: укажите имя (cloudsync accounts)", server)
	}
	a := match[0]
	if err := s.Logout(context.Background(), a.Server, a.UserID, signin.LogoutOptions{Force: *force}); err != nil {
		return err
	}
	fmt.Printf("Выход выполнен: %s на %s.\n", accountName(a), a.Server)
	return nil
}

func runAccounts(args []string) error {
	if len(args) != 0 {
		usage()
	}
	s, err := store()
	if err != nil {
		return err
	}
	accounts, err := s.Accounts()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("Аккаунтов нет. Войдите: cloudsync login <адрес>")
		return nil
	}
	for _, a := range accounts {
		where := "связка ключей"
		if a.TokenInFile {
			where = "файл"
		}
		fmt.Printf("%s\t%s\tустройство %q\tтокен: %s\tс %s\n", a.Server, accountName(a), a.DeviceName, where, a.SignedInAt.Local().Format("2006-01-02 15:04"))
	}
	return nil
}

func accountName(a signin.Account) string {
	if a.DisplayName != "" && a.DisplayName != a.Username {
		return a.DisplayName + " (" + a.Username + ")"
	}
	return a.Username
}
