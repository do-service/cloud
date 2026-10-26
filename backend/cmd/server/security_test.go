package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/config"
	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/jobs"
	"github.com/do-service/cloud/backend/internal/share"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
)

// The security matrix (E6-5): every protocol against every kind of access,
// on the server as newApp assembles it for production — the same routes,
// middleware and services — so a route wired without its check shows up
// here. Each cell is a request and the outcome it must have; the cells
// also make docs/security-matrix.md, which -update rewrites and which
// otherwise must match.

var update = flag.Bool("update", false, "rewrite docs/security-matrix.md")

const matrixDoc = "../../../docs/security-matrix.md"

// world is Alice's files, shared every way there is.
//
//	Проект/            shared with carol (read), dave (write), group Отдел
//	                   (erin, read), gina (read+reshare), hank (revoked),
//	                   ivan (write, then disabled)
//	Проект/план.txt, Проект/отчёт.docx, Проект/фото.jpg
//	Проект/Секрет/     shared with frank (read) alone
//	Проект/Секрет/ключ.txt
//	вне.txt            in Alice's root, shared with no one
type world struct {
	t       *testing.T
	h       http.Handler
	pool    *pgxpool.Pool
	files   *fs.Service
	ids     map[string]string // username → id
	session map[string]string // username → session cookie
	appPW   map[string]string // username → app password
	device  map[string]string // device column → device token
	wopi    map[string]string // username → WOPI token for отчёт.docx
	link    map[string]string // kind → link token
	node    map[string]string // name → node id
	n       int
}

// subjects are who asks, in the matrix's column order.
var subjects = []struct{ key, title string }{
	{"alice", "владелец"},
	{"root", "админ"},
	{"bob", "посторонний"},
	{"carol", "чтение"},
	{"dave", "запись"},
	{"erin", "группа (чтение)"},
	{"frank", "только подпапка"},
	{"gina", "чтение + делиться"},
	{"hank", "отозван"},
	{"ivan", "заблокирован"},
	{"device:alice", "устройство владельца"},
	{"device:root", "устройство админа"},
	{"device:revoked", "устройство отозвано"},
	{"", "аноним"},
}

func newWorld(t *testing.T) *world {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
	}
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, err := db.NewPool(ctx, url, 16, 0, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions, groups, audit_log CASCADE`); err != nil {
		t.Fatal(err)
	}

	// Previews by the fake vips the preview tests use.
	bin := t.TempDir()
	fake, err := filepath.Abs("../../internal/preview/testdata/fake-vips")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fake, filepath.Join(bin, "vips")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_VIPS_RUNS", filepath.Join(bin, "runs"))
	// An editor whose discovery opens .docx, without proof keys: what is
	// checked here is access, not signatures (internal/wopi tests those).
	editor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<wopi-discovery><net-zone name="external-http"><app name="writer">`+
			`<action ext="docx" name="edit" urlsrc="http://office.test/cool.html?"/></app></net-zone></wopi-discovery>`)
	}))
	t.Cleanup(editor.Close)

	cfg := config.Config{
		DataDir: t.TempDir(), PublicBaseURL: "http://cloud.test", SessionTTL: time.Hour, Role: config.RoleAPI,
		SecretKey: []byte(strings.Repeat("k", config.SecretKeyLen)), WOPIDiscoveryURL: editor.URL, WOPIHostURL: "http://cloud.test",
	}
	store, err := localfs.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a, err := newApp(ctx, cfg, pool, store)
	if err != nil {
		t.Fatal(err)
	}

	w := &world{t: t, h: a.handler, pool: pool, files: fs.New(pool, store),
		ids: map[string]string{}, session: map[string]string{}, appPW: map[string]string{}, device: map[string]string{}, wopi: map[string]string{},
		link: map[string]string{}, node: map[string]string{}}
	sessions, apps := auth.NewSessions(pool, time.Hour), auth.NewAppPasswords(pool)
	for _, s := range subjects {
		if s.key == "" || strings.HasPrefix(s.key, "device:") {
			continue
		}
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users (username, is_admin) VALUES ($1, $2) RETURNING id`, s.key, s.key == "root").Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := w.files.EnsureRoot(ctx, id); err != nil {
			t.Fatal(err)
		}
		w.ids[s.key] = id
		if w.session[s.key], _, err = sessions.Create(ctx, id, netip.Addr{}, ""); err != nil {
			t.Fatal(err)
		}
		if _, w.appPW[s.key], err = apps.Create(ctx, id, "matrix"); err != nil {
			t.Fatal(err)
		}
	}

	// Device tokens, as a desktop client holds them after signing in
	// (E12): Alice's, the admin's, and one of Alice's revoked.
	for _, key := range []string{"device:alice", "device:root", "device:revoked"} {
		w.device[key] = w.newDevice(strings.Replace(strings.TrimPrefix(key, "device:"), "revoked", "alice", 1))
	}
	if _, err := pool.Exec(ctx, `DELETE FROM devices WHERE token_hash = sha256(convert_to($1, 'UTF8'))`, w.device["device:revoked"]); err != nil {
		t.Fatal(err)
	}

	alice := w.ids["alice"]
	root, _ := w.files.Root(ctx, alice)
	mk := func(name, parent string) string {
		n, err := w.files.CreateDir(ctx, alice, parent, name)
		if err != nil {
			t.Fatal(err)
		}
		w.node[name] = n.ID
		return n.ID
	}
	put := func(name, parent, content string) {
		n, err := w.files.CreateFile(ctx, alice, parent, name, strings.NewReader(content), int64(len(content)))
		if err != nil {
			t.Fatal(err)
		}
		w.node[name] = n.ID
	}
	w.node["корень"] = root.ID
	project := mk("Проект", root.ID)
	secret := mk("Секрет", project)
	put("план.txt", project, "план")
	put("отчёт.docx", project, "docx")
	put("фото.jpg", project, "jpeg")
	put("ключ.txt", secret, "ключ")
	put("вне.txt", root.ID, "вне")

	shares := share.New(pool, w.files)
	grant := func(user, node string, p access.Perm) string {
		s, err := shares.Grant(ctx, alice, node, share.GranteeUser, w.ids[user], p)
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	grant("carol", project, access.Read)
	grant("dave", project, access.Read|access.Write)
	grant("frank", secret, access.Read)
	grant("gina", project, access.Read|access.Reshare)
	hanks := grant("hank", project, access.Read)
	grant("ivan", project, access.Read|access.Write)
	group, err := shares.CreateGroup(ctx, "Отдел")
	if err != nil {
		t.Fatal(err)
	}
	if err := shares.SetMember(ctx, group.ID, w.ids["erin"], true); err != nil {
		t.Fatal(err)
	}
	if _, err := shares.Grant(ctx, alice, project, share.GranteeGroup, group.ID, access.Read); err != nil {
		t.Fatal(err)
	}

	// Editor tokens, through the API as the web UI gets them: Hank's
	// before his share goes.
	for _, u := range []string{"alice", "carol", "dave", "hank"} {
		rec := w.do("POST", "/api/v1/nodes/"+w.node["отчёт.docx"]+"/edit", "", w.cookie(u)...)
		var res struct {
			AccessToken string `json:"accessToken"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.AccessToken == "" {
			t.Fatalf("edit as %s: %d %s", u, rec.Code, rec.Body)
		}
		w.wopi[u] = res.AccessToken
	}
	if err := shares.Revoke(ctx, alice, hanks); err != nil {
		t.Fatal(err)
	}
	// Ivan is disabled with his session and app password still alive.
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, w.ids["ivan"]); err != nil {
		t.Fatal(err)
	}

	box, err := auth.NewSecretBox(cfg.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	links := share.NewLinks(shares, box)
	for _, l := range []struct{ kind, mode, password string }{
		{"view", share.ModeView, ""}, {"download", share.ModeDownload, ""}, {"upload", share.ModeUpload, ""},
		{"password", share.ModeView, "секрет"}, {"expired", share.ModeView, ""}, {"deleted", share.ModeView, ""},
	} {
		link, err := links.Create(ctx, alice, project, l.mode, l.password, nil)
		if err != nil {
			t.Fatal(err)
		}
		w.link[l.kind] = link.Token
		switch l.kind {
		case "expired":
			if _, err := pool.Exec(ctx, `UPDATE links SET expires_at = now() - interval '1 minute' WHERE id = $1`, link.ID); err != nil {
				t.Fatal(err)
			}
		case "deleted":
			if err := links.Delete(ctx, alice, link.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	return w
}

// cookie is the column's credential: a session cookie, or for a device
// column its bearer token.
func (w *world) cookie(user string) []string {
	if user == "" {
		return nil
	}
	if tok, ok := w.device[user]; ok {
		return []string{"Authorization", "Bearer " + tok}
	}
	return []string{"Cookie", "session=" + w.session[user]}
}

// newDevice signs a device of user in, as the authorize flow (E12-2)
// will, and returns its token.
func (w *world) newDevice(user string) string {
	_, tok, err := auth.NewDevices(w.pool).Create(context.Background(), w.ids[user], "matrix", "linux", "0")
	if err != nil {
		w.t.Fatal(err)
	}
	return tok
}

func (w *world) basic(user string) []string {
	if user == "" {
		return nil
	}
	return []string{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+w.appPW[user]))}
}

// do sends a request; hdr are name, value pairs.
func (w *world) do(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" && strings.HasPrefix(body, "{") {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	return rec
}

func status(rec *httptest.ResponseRecorder) string { return fmt.Sprint(rec.Code) }

// unique is a fresh name, so writes don't clash.
func (w *world) unique(ext string) string {
	w.n++
	return fmt.Sprintf("n%d%s", w.n, ext)
}

// victim is a fresh file of Alice's in Проект, for a cell that changes or
// deletes what it acts on, so no cell depends on another.
func (w *world) victim() string {
	ctx := context.Background()
	n, err := w.files.CreateFile(ctx, w.ids["alice"], w.node["Проект"], w.unique(".txt"), strings.NewReader("x"), 1)
	if err != nil {
		w.t.Fatal(err)
	}
	return n.ID
}

// batch runs one operation of the batch API; its outcome is "ok" or the
// error code (the batch itself answers 200).
func (w *world) batch(user, op string) string {
	rec := w.do("POST", "/api/v1/nodes/batch", `{"ops":[`+op+`]}`, w.cookie(user)...)
	if rec.Code != http.StatusOK {
		return status(rec)
	}
	var out struct {
		Results []struct {
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Results) != 1 {
		return "bad response"
	}
	if e := out.Results[0].Error; e != nil {
		return e.Code
	}
	return "ok"
}

func tusMetadata(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, pairs[i]+" "+base64.StdEncoding.EncodeToString([]byte(pairs[i+1])))
	}
	return strings.Join(parts, ",")
}

// dav is the WebDAV path of name in user's tree; shared, under the
// folder of what others shared.
func dav(user string, shared bool, name ...string) string {
	if user == "" {
		user = "alice"
	}
	p := "/dav/files/" + user + "/"
	if shared {
		p += "Доступно мне/"
	}
	return (&url.URL{Path: p + strings.Join(name, "/")}).EscapedPath()
}

// row is one operation of a table: a title and how each column fares.
type row struct {
	title string
	cell  func(w *world, col string) string
}

type table struct {
	title   string
	columns []struct{ key, title string }
	rows    []row
	// want is each row's expected outcomes, in column order.
	want map[string][]string
}

func (w *world) restTable() table {
	node := func(name string) string { return w.node[name] }
	get := func(path func() string) func(*world, string) string {
		return func(w *world, u string) string { return status(w.do("GET", path(), "", w.cookie(u)...)) }
	}
	// seen: a file Alice adds to the folder after the caller's cursor
	// comes back in their changes.
	seen := func(folder string) func(*world, string) string {
		return func(w *world, u string) string {
			rec := w.do("GET", "/api/v1/sync/roots", "", w.cookie(u)...)
			var roots struct{ Cursor string }
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &roots) != nil {
				return status(rec)
			}
			n, err := w.files.CreateFile(context.Background(), w.ids["alice"], node(folder), w.unique(".txt"), strings.NewReader("x"), 1)
			if err != nil {
				w.t.Fatal(err)
			}
			rec = w.do("GET", "/api/v1/sync/changes?cursor="+url.QueryEscape(roots.Cursor), "", w.cookie(u)...)
			if rec.Code != http.StatusOK {
				return status(rec)
			}
			if strings.Contains(rec.Body.String(), `"id":"`+n.ID+`"`) {
				return "да"
			}
			return "нет"
		}
	}
	// revoke deletes a spare device of Alice's at path(its id), then drops
	// whatever is left of it: each cell starts from the same devices, and
	// refused ones don't pile up to the limit.
	revoke := func(path func(id string) string) func(*world, string) string {
		return func(w *world, u string) string {
			ctx := context.Background()
			dev, _, err := auth.NewDevices(w.pool).Create(ctx, w.ids["alice"], "spare", "linux", "0")
			if err != nil {
				w.t.Fatal(err)
			}
			out := status(w.do("DELETE", path(dev.ID), "", w.cookie(u)...))
			if _, err := w.pool.Exec(ctx, `DELETE FROM devices WHERE id = $1`, dev.ID); err != nil {
				w.t.Fatal(err)
			}
			return out
		}
	}
	return table{
		title:   "REST API (`/api/v1`), вход по сессии или токену устройства",
		columns: subjects,
		rows: []row{
			{"Папка: метаданные", get(func() string { return "/api/v1/nodes/" + node("Проект") })},
			{"Папка: листинг", get(func() string { return "/api/v1/nodes/" + node("Проект") + "/children" })},
			{"Файл: скачать", get(func() string { return "/api/v1/nodes/" + node("план.txt") + "/content" })},
			{"Папка: ZIP", get(func() string { return "/api/v1/nodes/" + node("Проект") + "/archive" })},
			{"Файл: превью", get(func() string { return "/api/v1/nodes/" + node("фото.jpg") + "/preview" })},
			{"Файл: версии", get(func() string { return "/api/v1/nodes/" + node("план.txt") + "/versions" })},
			{"Файл в подпапке", get(func() string { return "/api/v1/nodes/" + node("ключ.txt") + "/content" })},
			{"Файл вне общего", get(func() string { return "/api/v1/nodes/" + node("вне.txt") + "/content" })},
			{"Новая папка", func(w *world, u string) string {
				return status(w.do("POST", "/api/v1/nodes/"+node("Проект")+"/children", `{"name":"`+w.unique("")+`"}`, w.cookie(u)...))
			}},
			{"Загрузка (tus)", func(w *world, u string) string {
				return status(w.do("POST", "/api/v1/uploads/", "", append(w.cookie(u), "Tus-Resumable", "1.0.0", "Upload-Length", "1",
					"Upload-Metadata", tusMetadata("filename", w.unique(".bin"), "parentId", node("Проект")))...))
			}},
			{"Новая версия по id (tus nodeId)", func(w *world, u string) string {
				return status(w.do("POST", "/api/v1/uploads/", "", append(w.cookie(u), "Tus-Resumable", "1.0.0", "Upload-Length", "1",
					"Upload-Metadata", tusMetadata("nodeId", node("план.txt")))...))
			}},
			{"Переименование", func(w *world, u string) string {
				return status(w.do("PATCH", "/api/v1/nodes/"+w.victim(), `{"name":"`+w.unique(".txt")+`"}`, w.cookie(u)...))
			}},
			{"Удаление", func(w *world, u string) string {
				return status(w.do("DELETE", "/api/v1/nodes/"+w.victim(), "", w.cookie(u)...))
			}},
			{"Перемещение внутри", func(w *world, u string) string {
				return w.batch(u, `{"op":"move","id":"`+w.victim()+`","parentId":"`+node("Секрет")+`"}`)
			}},
			{"Копирование к себе", func(w *world, u string) string {
				return w.batch(u, `{"op":"copy","id":"`+node("план.txt")+`","parentId":"root"}`)
			}},
			{"Поделиться (с bob)", func(w *world, u string) string {
				out := status(w.do("POST", "/api/v1/nodes/"+node("Проект")+"/shares",
					`{"granteeType":"user","granteeId":"`+w.ids["bob"]+`","permissions":["read"]}`, w.cookie(u)...))
				if _, err := w.pool.Exec(context.Background(), `DELETE FROM shares WHERE grantee_id = $1`, w.ids["bob"]); err != nil {
					w.t.Fatal(err)
				}
				return out
			}},
			{"Создать ссылку", func(w *world, u string) string {
				return status(w.do("POST", "/api/v1/nodes/"+node("Проект")+"/links", `{"mode":"view"}`, w.cookie(u)...))
			}},
			{"Открыть в редакторе", func(w *world, u string) string {
				return status(w.do("POST", "/api/v1/nodes/"+node("отчёт.docx")+"/edit", "", w.cookie(u)...))
			}},
			{"Поиск находит чужое", func(w *world, u string) string {
				rec := w.do("GET", "/api/v1/search?q="+url.QueryEscape("план"), "", w.cookie(u)...)
				if rec.Code != http.StatusOK {
					return status(rec)
				}
				if strings.Contains(rec.Body.String(), node("план.txt")) {
					return "да"
				}
				return "нет"
			}},
			{"Синхронизация: дерево «Проект»", get(func() string { return "/api/v1/sync/tree?root=" + node("Проект") })},
			{"Синхронизация: «Проект» среди корней", func(w *world, u string) string {
				rec := w.do("GET", "/api/v1/sync/roots", "", w.cookie(u)...)
				if rec.Code != http.StatusOK {
					return status(rec)
				}
				if strings.Contains(rec.Body.String(), `"id":"`+node("Проект")+`"`) {
					return "да"
				}
				return "нет"
			}},
			{"Синхронизация: изменения в «Проект» видны", func(w *world, u string) string {
				rec := w.do("GET", "/api/v1/sync/roots", "", w.cookie(u)...)
				var roots struct{ Cursor string }
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &roots) != nil {
					return status(rec)
				}
				id := w.victim()
				rec = w.do("GET", "/api/v1/sync/changes?cursor="+url.QueryEscape(roots.Cursor), "", w.cookie(u)...)
				if rec.Code != http.StatusOK {
					return status(rec)
				}
				if strings.Contains(rec.Body.String(), `"id":"`+id+`"`) {
					return "да"
				}
				return "нет"
			}},
			{"Синхронизация: изменения в «Секрет» видны", seen("Секрет")},
			{"Синхронизация: изменения вне общего видны", seen("корень")},
			{"Синхронизация: дерево «Секрет»", get(func() string { return "/api/v1/sync/tree?root=" + node("Секрет") })},
			{"Синхронизация: дерево корня владельца", get(func() string { return "/api/v1/sync/tree?root=" + node("корень") })},
			{"Админка: пользователи", get(func() string { return "/api/v1/admin/users" })},
			{"Аккаунт: пароли приложений", get(func() string { return "/api/v1/me/app-passwords" })},
			{"Аккаунт: устройства", get(func() string { return "/api/v1/me/devices" })},
			{"Аккаунт: отозвать устройство владельца", revoke(func(id string) string { return "/api/v1/me/devices/" + id })},
			{"Админка: устройства владельца", get(func() string { return "/api/v1/admin/users/" + w.ids["alice"] + "/devices" })},
			{"Админка: отозвать устройство владельца", revoke(func(id string) string {
				return "/api/v1/admin/users/" + w.ids["alice"] + "/devices/" + id
			})},
			{"Выход устройства", func(w *world, u string) string {
				// Each live device signs out with a token of its own, so
				// the column's token keeps working for the rows after.
				hdr := w.cookie(u)
				if u == "device:alice" || u == "device:root" {
					hdr = []string{"Authorization", "Bearer " + w.newDevice(strings.TrimPrefix(u, "device:"))}
				}
				return status(w.do("DELETE", "/api/v1/me/devices/current", "", hdr...))
			}},
			{"Вход устройства: согласие", func(w *world, u string) string {
				body := `{"redirectUri":"http://127.0.0.1:53682/cb","codeChallenge":"` + strings.Repeat("A", 43) +
					`","codeChallengeMethod":"S256","state":"s","name":"matrix","platform":"linux","clientVersion":"0"}`
				return status(w.do("POST", "/api/v1/devices/authorize", body, w.cookie(u)...))
			}},
			{"Вход устройства: обмен чужого кода", func(w *world, u string) string {
				// From an address of its own each time: the exchange is
				// limited per IP, and a column's verdict mustn't depend on
				// how many came before it.
				w.n++
				req := httptest.NewRequest("POST", "/api/v1/devices/token",
					strings.NewReader(`{"code":"`+strings.Repeat("c", 43)+`","codeVerifier":"`+strings.Repeat("v", 43)+`","redirectUri":"http://127.0.0.1:53682/cb"}`))
				req.RemoteAddr = fmt.Sprintf("198.51.100.%d:40000", w.n%250+1)
				req.Header.Set("Content-Type", "application/json")
				if c := w.cookie(u); c != nil {
					req.Header.Set(c[0], c[1])
				}
				rec := httptest.NewRecorder()
				w.h.ServeHTTP(rec, req)
				return status(rec)
			}},
		},
		want: map[string][]string{
			//                         владелец админ посторонний чтение запись группа подпапка делиться отозван заблок устр-влад устр-админ устр-отозв аноним
			"Папка: метаданные":                    {"200", "404", "404", "200", "200", "200", "404", "200", "404", "401", "200", "404", "401", "401"},
			"Папка: листинг":                       {"200", "404", "404", "200", "200", "200", "404", "200", "404", "401", "200", "404", "401", "401"},
			"Файл: скачать":                        {"200", "404", "404", "200", "200", "200", "404", "200", "404", "401", "200", "404", "401", "401"},
			"Папка: ZIP":                           {"200", "404", "404", "200", "200", "200", "404", "200", "404", "401", "200", "404", "401", "401"},
			"Файл: превью":                         {"200", "404", "404", "200", "200", "200", "404", "200", "404", "401", "200", "404", "401", "401"},
			"Файл: версии":                         {"200", "404", "404", "200", "200", "200", "404", "200", "404", "401", "200", "404", "401", "401"},
			"Файл в подпапке":                      {"200", "404", "404", "200", "200", "200", "200", "200", "404", "401", "200", "404", "401", "401"},
			"Файл вне общего":                      {"200", "404", "404", "404", "404", "404", "404", "404", "404", "401", "200", "404", "401", "401"},
			"Новая папка":                          {"201", "404", "404", "403", "201", "403", "404", "403", "404", "401", "201", "404", "401", "401"},
			"Загрузка (tus)":                       {"201", "404", "404", "403", "201", "403", "404", "403", "404", "401", "201", "404", "401", "401"},
			"Новая версия по id (tus nodeId)":      {"201", "404", "404", "403", "201", "403", "404", "403", "404", "401", "201", "404", "401", "401"},
			"Переименование":                       {"200", "404", "404", "403", "200", "403", "404", "403", "404", "401", "200", "404", "401", "401"},
			"Удаление":                             {"204", "404", "404", "403", "204", "403", "404", "403", "404", "401", "204", "404", "401", "401"},
			"Перемещение внутри":                   {"ok", "not_found", "not_found", "forbidden", "ok", "forbidden", "not_found", "forbidden", "not_found", "401", "ok", "not_found", "401", "401"},
			"Копирование к себе":                   {"ok", "not_found", "not_found", "ok", "ok", "ok", "not_found", "ok", "not_found", "401", "ok", "not_found", "401", "401"},
			"Поделиться (с bob)":                   {"200", "404", "404", "403", "403", "403", "404", "200", "404", "401", "200", "404", "401", "401"},
			"Создать ссылку":                       {"201", "404", "404", "403", "403", "403", "404", "201", "404", "401", "201", "404", "401", "401"},
			"Открыть в редакторе":                  {"200", "404", "404", "200", "200", "200", "404", "200", "404", "401", "200", "404", "401", "401"},
			"Поиск находит чужое":                  {"да", "нет", "нет", "нет", "нет", "нет", "нет", "нет", "нет", "401", "да", "нет", "401", "401"},
			"Синхронизация: дерево «Проект»":       {"200", "404", "404", "200", "200", "200", "404", "200", "404", "401", "200", "404", "401", "401"},
			"Синхронизация: «Проект» среди корней": {"нет", "нет", "нет", "да", "да", "да", "нет", "да", "нет", "401", "нет", "нет", "401", "401"},
			"Синхронизация: изменения в «Проект» видны": {"да", "нет", "нет", "да", "да", "да", "нет", "да", "нет", "401", "да", "нет", "401", "401"},
			"Синхронизация: изменения в «Секрет» видны": {"да", "нет", "нет", "да", "да", "да", "да", "да", "нет", "401", "да", "нет", "401", "401"},
			"Синхронизация: изменения вне общего видны": {"да", "нет", "нет", "нет", "нет", "нет", "нет", "нет", "нет", "401", "да", "нет", "401", "401"},
			"Синхронизация: дерево «Секрет»":            {"200", "404", "404", "200", "200", "200", "200", "200", "404", "401", "200", "404", "401", "401"},
			"Синхронизация: дерево корня владельца":     {"200", "404", "404", "404", "404", "404", "404", "404", "404", "401", "200", "404", "401", "401"},
			"Админка: пользователи":                     {"403", "200", "403", "403", "403", "403", "403", "403", "403", "401", "403", "403", "401", "401"},
			// A device gets the files, not the account's credentials; with a
			// session there is no device to sign out.
			"Аккаунт: пароли приложений": {"200", "200", "200", "200", "200", "200", "200", "200", "200", "401", "403", "403", "401", "401"},
			"Аккаунт: устройства":        {"200", "200", "200", "200", "200", "200", "200", "200", "200", "401", "403", "403", "401", "401"},
			// Only the owner revokes from their settings, only an admin
			// from the admin's; a device token does neither.
			"Аккаунт: отозвать устройство владельца": {"204", "404", "404", "404", "404", "404", "404", "404", "404", "401", "403", "403", "401", "401"},
			"Админка: устройства владельца":          {"403", "200", "403", "403", "403", "403", "403", "403", "403", "401", "403", "403", "401", "401"},
			"Админка: отозвать устройство владельца": {"403", "204", "403", "403", "403", "403", "403", "403", "403", "401", "403", "403", "401", "401"},
			"Выход устройства":                       {"404", "404", "404", "404", "404", "404", "404", "404", "404", "401", "204", "204", "401", "401"},
			// Consent takes a person at a browser: a device can't sign in
			// another. The exchange needs no account, only a live code.
			"Вход устройства: согласие":          {"200", "200", "200", "200", "200", "200", "200", "200", "200", "401", "403", "403", "401", "401"},
			"Вход устройства: обмен чужого кода": {"400", "400", "400", "400", "400", "400", "400", "400", "400", "400", "400", "400", "400", "400"},
		},
	}
}

func (w *world) davTable() table {
	propfind := func(path func(u string) string) func(*world, string) string {
		return func(w *world, u string) string {
			return status(w.do("PROPFIND", path(u), "", append(w.basic(u), "Depth", "0")...))
		}
	}
	return table{
		title: "WebDAV (`/dav/files/{логин}/`), вход по паролю приложения",
		columns: []struct{ key, title string }{
			{"alice", "владелец"}, {"bob", "посторонний"}, {"carol", "чтение"}, {"dave", "запись"},
			{"frank", "только подпапка"}, {"hank", "отозван"}, {"ivan", "заблокирован"}, {"", "аноним"},
		},
		rows: []row{
			{"Дерево владельца по его адресу", propfind(func(string) string { return dav("alice", false, "Проект") + "/" })},
			{"«Доступно мне» / Проект", propfind(func(u string) string { return dav(u, true, "Проект") + "/" })},
			{"«Доступно мне» / Секрет", propfind(func(u string) string { return dav(u, true, "Секрет") + "/" })},
			{"Скачать через общее", func(w *world, u string) string {
				return status(w.do("GET", dav(u, true, "Проект", "план.txt"), "", w.basic(u)...))
			}},
			{"Записать через общее", func(w *world, u string) string {
				return status(w.do("PUT", dav(u, true, "Проект", w.unique(".txt")), "x", w.basic(u)...))
			}},
			{"Удалить через общее", func(w *world, u string) string {
				name := w.victimName()
				return status(w.do("DELETE", dav(u, true, "Проект", name), "", w.basic(u)...))
			}},
			{"Заблокировать (LOCK) через общее", func(w *world, u string) string {
				name := w.victimName()
				body := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
				return status(w.do("LOCK", dav(u, true, "Проект", name), body, w.basic(u)...))
			}},
			{"Выход из дерева через «..»", func(w *world, u string) string {
				return status(w.do("GET", "/dav/files/"+orAlice(u)+"/../alice/"+url.PathEscape("вне.txt"), "", w.basic(u)...))
			}},
		},
		want: map[string][]string{
			//                                    владелец посторонний чтение запись подпапка отозван заблок аноним
			"Дерево владельца по его адресу": {"207", "404", "404", "404", "404", "404", "401", "401"},
			"«Доступно мне» / Проект":        {"404", "404", "207", "207", "404", "404", "401", "401"},
			"«Доступно мне» / Секрет":        {"404", "404", "404", "404", "207", "404", "401", "401"},
			"Скачать через общее":            {"404", "404", "200", "200", "404", "404", "401", "401"},
			// No such folder in their tree: 409, as RFC 4918 has it for a
			// missing parent.
			"Записать через общее":             {"409", "409", "403", "201", "409", "409", "401", "401"},
			"Удалить через общее":              {"404", "404", "403", "204", "404", "404", "401", "401"},
			"Заблокировать (LOCK) через общее": {"409", "409", "403", "200", "409", "409", "401", "401"},
			"Выход из дерева через «..»":       {"307", "307", "307", "307", "307", "307", "307", "307"},
		},
	}
}

func orAlice(u string) string {
	if u == "" {
		return "alice"
	}
	return u
}

// victimName is victim, by name.
func (w *world) victimName() string {
	n, err := w.files.Get(context.Background(), w.ids["alice"], w.victim())
	if err != nil {
		w.t.Fatal(err)
	}
	return n.Name
}

func (w *world) wopiTable() table {
	file := func() string { return w.node["отчёт.docx"] }
	call := func(method string, path func() string, body string, hdr ...string) func(*world, string) string {
		return func(w *world, u string) string {
			return status(w.do(method, path()+"?access_token="+url.QueryEscape(w.wopi[u]), body, hdr...))
		}
	}
	return table{
		title: "WOPI (`/wopi/files/{id}`), токен из «Открыть в редакторе»",
		columns: []struct{ key, title string }{
			{"alice", "владелец"}, {"carol", "чтение"}, {"dave", "запись"}, {"hank", "отозван после выдачи"}, {"", "без токена"},
		},
		rows: []row{
			{"CheckFileInfo", call("GET", func() string { return "/wopi/files/" + file() }, "")},
			{"Можно ли писать (UserCanWrite)", func(w *world, u string) string {
				rec := w.do("GET", "/wopi/files/"+file()+"?access_token="+url.QueryEscape(w.wopi[u]), "")
				if rec.Code != http.StatusOK {
					return status(rec)
				}
				var fi struct{ UserCanWrite bool }
				_ = json.Unmarshal(rec.Body.Bytes(), &fi)
				if fi.UserCanWrite {
					return "да"
				}
				return "нет"
			}},
			{"GetFile", call("GET", func() string { return "/wopi/files/" + file() + "/contents" }, "")},
			{"PutFile", call("POST", func() string { return "/wopi/files/" + file() + "/contents" }, "new", "X-WOPI-Override", "PUT")},
			{"LOCK", func(w *world, u string) string {
				out := status(w.do("POST", "/wopi/files/"+file()+"?access_token="+url.QueryEscape(w.wopi[u]), "", "X-WOPI-Override", "LOCK", "X-WOPI-Lock", "L-"+u))
				// Let the next column lock it too.
				if _, err := w.pool.Exec(context.Background(), `DELETE FROM locks`); err != nil {
					w.t.Fatal(err)
				}
				return out
			}},
			{"Токен на другой файл", call("GET", func() string { return "/wopi/files/" + w.node["план.txt"] }, "")},
		},
		want: map[string][]string{
			//                               владелец чтение запись отозван без-токена
			"CheckFileInfo": {"200", "200", "200", "404", "401"},
			"Можно ли писать (UserCanWrite)": {"да", "нет", "да", "404", "401"},
			"GetFile":              {"200", "200", "200", "404", "401"},
			"PutFile":              {"200", "401", "200", "404", "401"},
			"LOCK":                 {"200", "401", "200", "404", "401"},
			"Токен на другой файл": {"401", "401", "401", "401", "401"},
		},
	}
}

func (w *world) linkTable() table {
	in := func(kind, path string) string { return "/s/" + w.link[kind] + "/api" + path }
	get := func(path func() string) func(*world, string) string {
		return func(w *world, kind string) string { return status(w.do("GET", in(kind, path()), "")) }
	}
	return table{
		title: "Публичные ссылки на «Проект» (`/s/{token}/api`), без входа",
		columns: []struct{ key, title string }{
			{"view", "просмотр"}, {"download", "скачивание"}, {"upload", "только загрузка"},
			{"password", "с паролем, не введён"}, {"expired", "просрочена"}, {"deleted", "удалена"},
		},
		rows: []row{
			{"Сведения о ссылке", get(func() string { return "/info" })},
			{"Листинг", get(func() string { return "/nodes/root/children" })},
			{"Файл (текст)", get(func() string { return "/nodes/" + w.node["план.txt"] + "/content" })},
			{"Файл в подпапке", get(func() string { return "/nodes/" + w.node["ключ.txt"] + "/content" })},
			{"Файл вне ссылки", get(func() string { return "/nodes/" + w.node["вне.txt"] + "/content" })},
			{"ZIP", get(func() string { return "/nodes/root/archive" })},
			{"Превью", get(func() string { return "/nodes/" + w.node["фото.jpg"] + "/preview" })},
			{"Загрузка (tus)", func(w *world, kind string) string {
				return status(w.do("POST", in(kind, "/uploads/"), "", "Tus-Resumable", "1.0.0", "Upload-Length", "1",
					"Upload-Metadata", tusMetadata("filename", w.unique(".bin"))))
			}},
			{"Новая версия по id (tus nodeId)", func(w *world, kind string) string {
				return status(w.do("POST", in(kind, "/uploads/"), "", "Tus-Resumable", "1.0.0", "Upload-Length", "1",
					"Upload-Metadata", tusMetadata("nodeId", w.node["план.txt"])))
			}},
		},
		want: map[string][]string{
			//                    просмотр скачивание загрузка пароль просрочена удалена
			"Сведения о ссылке":               {"200", "200", "200", "200", "404", "404"},
			"Листинг":                         {"200", "200", "403", "401", "404", "404"},
			"Файл (текст)":                    {"200", "200", "403", "401", "404", "404"},
			"Файл в подпапке":                 {"200", "200", "403", "401", "404", "404"},
			"Файл вне ссылки":                 {"404", "404", "403", "401", "404", "404"},
			"ZIP":                             {"403", "200", "403", "401", "404", "404"},
			"Превью":                          {"200", "200", "403", "401", "404", "404"},
			"Загрузка (tus)":                  {"403", "403", "201", "401", "404", "404"},
			"Новая версия по id (tus nodeId)": {"403", "403", "400", "401", "404", "404"},
		},
	}
}

func TestSecurityMatrix(t *testing.T) {
	w := newWorld(t)
	var doc strings.Builder
	doc.WriteString(`# Матрица доступа

Сгенерировано тестом ` + "`TestSecurityMatrix`" + ` (` + "`backend/cmd/server/security_test.go`" + `, E6-5) на сервере, собранном как в продакшене (` + "`newApp`" + `). Не править руками: ` + "`go test ./cmd/server -run SecurityMatrix -update`" + ` перезаписывает файл, а без флага тест падает, если поведение разошлось с ним.

Данные: у владельца папка «Проект» (общая для «чтение», «запись», группы, «чтение + делиться»; у «отозван» доступ был и отозван, «заблокирован» имел запись и заблокирован с живыми сессией и паролем приложения) с подпапкой «Секрет» (общей только для «только подпапка») и файл «вне.txt» в корне, не общий ни с кем. «Админ» — администратор без доступа к файлам. «Устройство владельца» и «устройство админа» — токены программы синхронизации (` + "`Authorization: Bearer cld_…`" + `) тех же пользователей, «устройство отозвано» — токен владельца после отзыва.

Коды: 200/201/204/207 — сделано; 401 — не вошёл (или заблокирован); 403 — видно, но прав нет; 404 — для этого пользователя объекта нет (чужое не отличить от несуществующего); 307 — путь с «..» перенаправлен на очищенный, без доступа к чему-либо; 409 в WebDAV — родительской папки у этого пользователя нет (RFC 4918). В пакетных операциях — код ошибки операции.
`)
	failed := 0
	for _, tb := range []table{w.restTable(), w.davTable(), w.wopiTable(), w.linkTable()} {
		doc.WriteString("\n## " + tb.title + "\n\n|  |")
		for _, c := range tb.columns {
			doc.WriteString(" " + c.title + " |")
		}
		doc.WriteString("\n|---|" + strings.Repeat("---|", len(tb.columns)) + "\n")
		for _, r := range tb.rows {
			want, ok := tb.want[r.title]
			if !ok || len(want) != len(tb.columns) {
				t.Fatalf("%s: %q has no expectations for each column", tb.title, r.title)
			}
			doc.WriteString("| " + r.title + " |")
			for i, c := range tb.columns {
				got := r.cell(w, c.key)
				cell := got
				if got != want[i] {
					failed++
					t.Errorf("%s / %s / %s: got %s, want %s", tb.title, r.title, c.title, got, want[i])
					cell = "**" + got + "** (ждали " + want[i] + ")"
				}
				doc.WriteString(" " + cell + " |")
			}
			doc.WriteString("\n")
		}
	}
	if failed > 0 {
		return
	}
	got := doc.String()
	if *update {
		if err := os.WriteFile(matrixDoc, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	old, err := os.ReadFile(matrixDoc)
	if err != nil || string(old) != got {
		t.Fatalf("%s is out of date: go test ./cmd/server -run SecurityMatrix -update", matrixDoc)
	}
}
