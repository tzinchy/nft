// mymark: постоянные ссылки для NFC-меток (/t/{token} → 302), админка, кабинет клиента,
// оплата через ЮKassa, уведомления в Telegram и аналитика переходов.
package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	_ "modernc.org/sqlite"
)

//go:embed templates static
var assets embed.FS

var msk, _ = time.LoadLocation("Europe/Moscow")

// migrations применяются по порядку, номер последней хранится в PRAGMA user_version.
// Новые изменения схемы — только новым элементом в конце, старые не трогать.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY, email TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'admin');
	CREATE TABLE IF NOT EXISTS sessions(token TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, csrf TEXT NOT NULL, expires_at INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS businesses(id INTEGER PRIMARY KEY, name TEXT NOT NULL, plan TEXT NOT NULL DEFAULT '', starts_at TEXT NOT NULL, expires_at TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS locations(id INTEGER PRIMARY KEY, business_id INTEGER NOT NULL REFERENCES businesses(id) ON DELETE CASCADE, name TEXT NOT NULL, address TEXT NOT NULL DEFAULT '');
	CREATE TABLE IF NOT EXISTS tags(id INTEGER PRIMARY KEY, location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE, public_token TEXT UNIQUE NOT NULL, name TEXT NOT NULL, destination_url TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS tag_clicks(id INTEGER PRIMARY KEY, tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE, created_at INTEGER NOT NULL);
	CREATE INDEX IF NOT EXISTS tag_clicks_tag ON tag_clicks(tag_id, created_at);
	CREATE TABLE IF NOT EXISTS tag_history(id INTEGER PRIMARY KEY, tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE, user_id INTEGER REFERENCES users(id) ON DELETE SET NULL, old_url TEXT NOT NULL, new_url TEXT NOT NULL, created_at INTEGER NOT NULL);`,

	`ALTER TABLE users ADD COLUMN business_id INTEGER REFERENCES businesses(id) ON DELETE CASCADE;
	ALTER TABLE users ADD COLUMN tg_chat_id INTEGER;
	ALTER TABLE users ADD COLUMN tg_code TEXT;
	ALTER TABLE businesses ADD COLUMN price INTEGER NOT NULL DEFAULT 1499;
	ALTER TABLE tag_clicks ADD COLUMN device TEXT NOT NULL DEFAULT '';
	ALTER TABLE tag_clicks ADD COLUMN visitor TEXT NOT NULL DEFAULT '';
	CREATE TABLE payments(id INTEGER PRIMARY KEY, business_id INTEGER NOT NULL REFERENCES businesses(id) ON DELETE CASCADE, user_id INTEGER REFERENCES users(id) ON DELETE SET NULL, provider TEXT NOT NULL, external_id TEXT UNIQUE, amount INTEGER NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL, paid_at INTEGER);
	CREATE TABLE outbox(id INTEGER PRIMARY KEY, chat_id INTEGER NOT NULL, text TEXT NOT NULL, dedupe TEXT UNIQUE, created_at INTEGER NOT NULL, sent_at INTEGER, attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '');
	CREATE TABLE kv(k TEXT PRIMARY KEY, v TEXT NOT NULL);`,
}

var errMsgs = map[string]string{
	"url":     "Ссылка должна начинаться с http:// или https://",
	"date":    "Неверная дата",
	"empty":   "Заполните обязательные поля",
	"exists":  "Пользователь с такой почтой уже есть",
	"pw":      "Нужна почта и пароль от 8 символов",
	"oldpw":   "Неверный текущий пароль",
	"tg":      "Telegram сейчас недоступен, попробуйте позже",
	"nopay":   "Онлайн-оплата пока не подключена — свяжитесь с нами",
	"payfail": "Не удалось создать платёж, попробуйте позже",
	"amount":  "Неверная сумма",
}

var okMsgs = map[string]string{
	"pw":     "Пароль изменён",
	"paid":   "Оплата отмечена, подписка продлена",
	"client": "Доступ клиента создан",
	"tgoff":  "Telegram отвязан",
}

type config struct {
	DBPath, Public, Panel string
	Contact               string // ссылка «Связаться» на главной (Telegram, WhatsApp, почта)
	YK                    ykConfig
	TGToken, TGAPI        string
}

type app struct {
	db            *sql.DB
	tpl           map[string]*template.Template
	public, panel string
	contact       string
	salt          string // для обезличенного подсчёта уникальных посетителей
	yk            ykConfig
	tg            *telegram // nil — Telegram выключен
	http          *http.Client
	now           func() time.Time
}

type session struct {
	Token      string
	UserID     int64
	Email      string
	CSRF       string
	Role       string // admin | client
	BusinessID int64  // для клиента
	TGLinked   bool
}

type status struct{ Class, Label string }

type business struct {
	ID                              int64
	Name, Plan, StartsAt, ExpiresAt string
	Price                           int
	Locations, Tags, Taps30         int
	Status                          status
}

type location struct {
	ID, BusinessID int64
	Name, Address  string
	Tags           int
	TagList        []tag
}

type tag struct {
	ID, LocationID   int64
	Token, Name, URL string
	Active           bool
	Clicks           int
}

type change struct {
	ID       int64
	Old, New string
	Author   string
	At       int64
}

func main() {
	addr := env("ADDR", ":8080")
	a, h, err := newApp(config{
		DBPath:  env("DB_PATH", "nfc.db"),
		Public:  env("PUBLIC_URL", "http://localhost:8080"),
		Panel:   env("PANEL_PATH", "/panel"),
		Contact: os.Getenv("CONTACT_URL"),
		YK: ykConfig{
			ShopID:  os.Getenv("YOOKASSA_SHOP_ID"),
			Secret:  os.Getenv("YOOKASSA_SECRET_KEY"),
			API:     env("YOOKASSA_API", "https://api.yookassa.ru/v3"),
			Receipt: os.Getenv("YOOKASSA_RECEIPT") == "1",
		},
		TGToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TGAPI:   env("TELEGRAM_API", "https://api.telegram.org"),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer a.db.Close()

	ctx := context.Background()
	go a.scheduler(ctx)
	if a.tg != nil {
		go a.tgPoll(ctx)
		go a.tgSender(ctx)
	}
	log.Printf("listening on %s, admin at %s/, cabinet at /cabinet/, yookassa=%v, telegram=%v", addr, a.panel, a.yk.enabled(), a.tg != nil)
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func newApp(c config) (*app, http.Handler, error) {
	db, err := sql.Open("sqlite", "file:"+c.DBPath+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, err
	}
	if err := migrate(db); err != nil {
		return nil, nil, err
	}
	a := &app{
		db: db, tpl: map[string]*template.Template{},
		public: strings.TrimRight(c.Public, "/"), panel: "/" + strings.Trim(c.Panel, "/"),
		contact: c.Contact, yk: c.YK, http: &http.Client{Timeout: 30 * time.Second}, now: time.Now,
	}
	if c.TGToken != "" {
		a.tg = &telegram{base: strings.TrimRight(c.TGAPI, "/") + "/bot" + c.TGToken, http: &http.Client{Timeout: 70 * time.Second}}
	}
	if a.salt = a.kv("salt"); a.salt == "" {
		a.salt = randHex(16)
		a.setKV("salt", a.salt)
	}

	funcs := template.FuncMap{
		"date":    ruDate,
		"dt":      func(u int64) string { return time.Unix(u, 0).In(msk).Format("02.01.2006 15:04") },
		"periods": func() []int { return []int{7, 30, 90} },
		"last":    func(b []bar) bar { return b[len(b)-1] },
		"json":    func(v any) (string, error) { b, err := json.Marshal(v); return string(b), err },
		"list":    func(v ...any) []any { return v },
		"plural":  plural,
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"payStatus": func(s string) status {
			return map[string]status{
				"succeeded": {"badge-ok", "Оплачено"}, "pending": {"badge-warn", "Ожидает"},
				"canceled": {"badge-bad", "Отменён"}, "error": {"badge-bad", "Ошибка"},
			}[s]
		},
	}
	pages, _ := fs.Glob(assets, "templates/*.html")
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".html")
		if name == "layout" || name == "partials" || name == "icons" {
			continue
		}
		a.tpl[name] = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/icons.html", p))
	}

	static, _ := fs.Sub(assets, "static")
	P := a.panel
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /t/{token}", a.redirect)
	mux.HandleFunc("POST /pay/yookassa", a.ykWebhook)
	mux.HandleFunc("GET /{$}", a.landing)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		a.message(w, 404, "Страница не найдена")
	})

	// админка
	mux.HandleFunc("GET "+P+"/{$}", a.dashboard)
	mux.HandleFunc("POST "+P+"/setup", a.setup)
	mux.HandleFunc("POST "+P+"/login", a.login)
	mux.HandleFunc("POST "+P+"/logout", a.anyUser(a.logout))
	mux.HandleFunc("POST "+P+"/telegram", a.admin(a.tgLink))
	mux.HandleFunc("POST "+P+"/telegram/off", a.admin(a.tgUnlink))
	mux.HandleFunc("POST "+P+"/businesses", a.admin(a.createBusiness))
	mux.HandleFunc("GET "+P+"/b/{id}", a.admin(a.businessPage))
	mux.HandleFunc("POST "+P+"/b/{id}", a.admin(a.updateBusiness))
	mux.HandleFunc("POST "+P+"/b/{id}/locations", a.admin(a.createLocation))
	mux.HandleFunc("POST "+P+"/b/{id}/clients", a.admin(a.createClient))
	mux.HandleFunc("POST "+P+"/b/{id}/paid", a.admin(a.manualPayment))
	mux.HandleFunc("POST "+P+"/users/{id}/delete", a.admin(a.deleteClient))
	mux.HandleFunc("GET "+P+"/l/{id}", a.admin(a.locationPage))
	mux.HandleFunc("POST "+P+"/l/{id}/tags", a.admin(a.createTag))
	mux.HandleFunc("GET "+P+"/g/{id}", a.admin(a.tagPage))
	mux.HandleFunc("POST "+P+"/g/{id}", a.admin(a.updateTag))
	mux.HandleFunc("POST "+P+"/g/{id}/revert/{hid}", a.admin(a.revertTag))

	// кабинет клиента
	mux.HandleFunc("GET /cabinet/{$}", a.cabinet)
	mux.HandleFunc("POST /cabinet/login", a.login)
	mux.HandleFunc("POST /cabinet/logout", a.anyUser(a.logout))
	mux.HandleFunc("GET /cabinet/g/{id}", a.client(a.cabinetTag))
	mux.HandleFunc("POST /cabinet/pay", a.client(a.startPayment))
	mux.HandleFunc("GET /cabinet/pay/{id}", a.client(a.paymentReturn))
	mux.HandleFunc("POST /cabinet/telegram", a.client(a.tgLink))
	mux.HandleFunc("POST /cabinet/telegram/off", a.client(a.tgUnlink))
	mux.HandleFunc("POST /cabinet/password", a.client(a.changePassword))
	return a, sameOrigin(mux), nil
}

// sameOrigin отбивает POST с чужих сайтов (дополнительно к CSRF-токену).
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); r.Method == http.MethodPost && o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// --- публичная часть ---

func (a *app) redirect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	var id int64
	var dst string
	var active bool
	err := a.db.QueryRowContext(r.Context(), `SELECT id, destination_url, active FROM tags WHERE public_token = ?`, r.PathValue("token")).Scan(&id, &dst, &active)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		a.message(w, 404, "Метка не найдена")
		return
	case err != nil:
		log.Print(err)
		a.message(w, 503, "Сервис временно недоступен")
		return
	case !active:
		a.message(w, 403, "Метка отключена")
		return
	}
	if ua := r.UserAgent(); !isBot(ua) { // превью ссылок в мессенджерах и роботы — не переходы
		now := a.now()
		_, err := a.db.ExecContext(r.Context(), `INSERT INTO tag_clicks(tag_id, created_at, device, visitor) VALUES(?, ?, ?, ?)`,
			id, now.Unix(), deviceOf(ua), a.visitor(clientIP(r), ua, now))
		if err != nil {
			log.Print(err) // переход важнее статистики
		}
	}
	http.Redirect(w, r, dst, http.StatusFound)
}

func isBot(ua string) bool {
	ua = strings.ToLower(ua)
	for _, s := range []string{"bot", "crawl", "spider", "preview", "facebookexternalhit", "whatsapp", "vkshare", "skype"} {
		if strings.Contains(ua, s) {
			return true
		}
	}
	return false
}

func deviceOf(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		return "ios"
	case strings.Contains(ua, "Android"):
		return "android"
	}
	return "other"
}

// visitor — обезличенный идентификатор посетителя на один день: IP и браузер не хранятся.
func (a *app) visitor(ip, ua string, now time.Time) string {
	h := sha256.Sum256([]byte(a.salt + "|" + ip + "|" + ua + "|" + now.In(msk).Format(time.DateOnly)))
	return hex.EncodeToString(h[:8])
}

// clientIP: за Caddy реальный адрес — последний в X-Forwarded-For (порт приложения наружу не открыт).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// landing — главная: что это за сервис, как работает и сколько стоит.
func (a *app) landing(w http.ResponseWriter, r *http.Request) {
	d := map[string]any{"Title": "Отзывы с одного касания"}
	if s := a.session(r); s != nil {
		d = a.data(r, s, "Отзывы с одного касания", d)
	}
	d["Public"], d["Index"], d["Contact"] = true, true, a.contact
	d["Description"] = "NFC-наклейки для кафе, салонов и магазинов: гость прикладывает телефон и сразу попадает на страницу отзывов. От 1 299 ₽ за метку в год."
	a.render(w, 200, "landing", d)
}

// --- вход ---

func (a *app) authPage(w http.ResponseWriter, code int, setup bool, action, errText string) {
	title := "Вход"
	if setup {
		title = "Создание администратора"
	}
	a.render(w, code, "auth", map[string]any{"Title": title, "Setup": setup, "Action": action, "Error": errText})
}

func (a *app) setup(w http.ResponseWriter, r *http.Request) {
	email, pw := strings.TrimSpace(r.PostFormValue("email")), r.PostFormValue("password")
	if _, err := mail.ParseAddress(email); err != nil || len(pw) < 8 {
		a.authPage(w, 400, true, a.panel+"/setup", errMsgs["pw"])
		return
	}
	// Вставка только если пользователей ещё нет — атомарно, без гонки двух первых запросов.
	res, err := a.db.Exec(`INSERT INTO users(email, password_hash, role) SELECT ?, ?, 'admin' WHERE NOT EXISTS (SELECT 1 FROM users)`, email, hashPassword(pw))
	if err != nil {
		a.fail(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Redirect(w, r, a.panel+"/", http.StatusSeeOther)
		return
	}
	id, _ := res.LastInsertId()
	a.startSession(w, r, id, "admin")
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	var id int64
	var hash, role string
	err := a.db.QueryRow(`SELECT id, password_hash, role FROM users WHERE email = ?`, strings.TrimSpace(r.PostFormValue("email"))).Scan(&id, &hash, &role)
	if err != nil || !checkPassword(hash, r.PostFormValue("password")) {
		time.Sleep(time.Second) // ponytail: тормоз перебора на запрос; нужен rate limit по IP, если вход начнут долбить
		a.authPage(w, 401, false, r.URL.Path, "Неверная почта или пароль")
		return
	}
	a.startSession(w, r, id, role)
}

func (a *app) startSession(w http.ResponseWriter, r *http.Request, uid int64, role string) {
	tok, csrf := randHex(32), randHex(16)
	exp := a.now().Add(30 * 24 * time.Hour)
	a.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, a.now().Unix())
	if _, err := a.db.Exec(`INSERT INTO sessions(token, user_id, csrf, expires_at) VALUES(?, ?, ?, ?)`, tok, uid, csrf, exp.Unix()); err != nil {
		a.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: tok, Path: "/", Expires: exp, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(a.public, "https://")})
	http.Redirect(w, r, a.home(role), http.StatusSeeOther)
}

func (a *app) home(role string) string {
	if role == "client" {
		return "/cabinet/"
	}
	return a.panel + "/"
}

func (a *app) session(r *http.Request) *session {
	// cookie «sid» может быть несколько (например, старая с другим Path) — берём первую рабочую
	for _, c := range r.CookiesNamed("sid") {
		var s session
		err := a.db.QueryRow(`SELECT s.user_id, u.email, s.csrf, u.role, COALESCE(u.business_id, 0), u.tg_chat_id IS NOT NULL
			FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token = ? AND s.expires_at > ?`, c.Value, a.now().Unix()).
			Scan(&s.UserID, &s.Email, &s.CSRF, &s.Role, &s.BusinessID, &s.TGLinked)
		if err == nil {
			s.Token = c.Value
			return &s
		}
	}
	return nil
}

type handler func(http.ResponseWriter, *http.Request, *session)

// guard пускает только вошедших с нужной ролью ("" — любой) и проверяет CSRF-токен на POST.
func (a *app) guard(role string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := a.session(r)
		if s == nil || (role != "" && s.Role != role) || (s.Role == "client" && s.BusinessID == 0) {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, a.home(role), http.StatusSeeOther)
			} else {
				a.message(w, 403, "Нужен вход")
			}
			return
		}
		if r.Method == http.MethodPost && subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.CSRF)) != 1 {
			a.message(w, 403, "Сессия устарела. Обновите страницу.")
			return
		}
		h(w, r, s)
	}
}

func (a *app) admin(h handler) http.HandlerFunc   { return a.guard("admin", h) }
func (a *app) client(h handler) http.HandlerFunc  { return a.guard("client", h) }
func (a *app) anyUser(h handler) http.HandlerFunc { return a.guard("", h) }

func (a *app) logout(w http.ResponseWriter, r *http.Request, s *session) {
	a.db.Exec(`DELETE FROM sessions WHERE token = ?`, s.Token)
	http.SetCookie(w, &http.Cookie{Name: "sid", Path: "/", MaxAge: -1})
	http.Redirect(w, r, a.home(s.Role), http.StatusSeeOther)
}

// --- общие операции с метками (админка и кабинет) ---

type tagView struct {
	T                          tag
	LocationName, BusinessName string
	BusinessID                 int64
}

// loadTag: biz > 0 ограничивает метками одного предприятия (кабинет клиента).
func (a *app) loadTag(id string, biz int64) (tagView, error) {
	var v tagView
	err := a.db.QueryRow(`SELECT t.id, t.location_id, t.public_token, t.name, t.destination_url, t.active, l.name, b.id, b.name
		FROM tags t JOIN locations l ON l.id = t.location_id JOIN businesses b ON b.id = l.business_id
		WHERE t.id = ? AND (? = 0 OR b.id = ?)`, id, biz, biz).
		Scan(&v.T.ID, &v.T.LocationID, &v.T.Token, &v.T.Name, &v.T.URL, &v.T.Active, &v.LocationName, &v.BusinessID, &v.BusinessName)
	return v, err
}

// saveTag меняет название, ссылку и статус метки, пишет историю смены ссылки.
func (a *app) saveTag(id string, uid int64, name, dst string, active bool) error {
	return a.tx(func(tx *sql.Tx) error {
		var old string
		err := tx.QueryRow(`SELECT destination_url FROM tags WHERE id = ?`, id).Scan(&old)
		if err != nil {
			return err
		}
		if old != dst {
			if _, err := tx.Exec(`INSERT INTO tag_history(tag_id, user_id, old_url, new_url, created_at) VALUES(?, ?, ?, ?, ?)`, id, uid, old, dst, a.now().Unix()); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`UPDATE tags SET name = ?, destination_url = ?, active = ? WHERE id = ?`, name, dst, active, id)
		return err
	})
}

// revertTag возвращает прежнюю ссылку из истории (только админ).
func (a *app) revertTag(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	err := a.tx(func(tx *sql.Tx) error {
		var prev, cur string
		err := tx.QueryRow(`SELECT h.old_url, t.destination_url FROM tag_history h JOIN tags t ON t.id = h.tag_id WHERE h.id = ? AND h.tag_id = ?`, r.PathValue("hid"), id).Scan(&prev, &cur)
		if err != nil || prev == cur {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tag_history(tag_id, user_id, old_url, new_url, created_at) VALUES(?, ?, ?, ?, ?)`, id, s.UserID, cur, prev, a.now().Unix()); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE tags SET destination_url = ? WHERE id = ?`, prev, id)
		return err
	})
	if err != nil {
		a.notFound(w, err)
		return
	}
	http.Redirect(w, r, a.panel+"/g/"+id, http.StatusSeeOther)
}

func (a *app) history(tagID int64) ([]change, error) {
	rows, err := a.db.Query(`SELECT h.id, h.old_url, h.new_url, COALESCE(u.email, '—'), h.created_at FROM tag_history h LEFT JOIN users u ON u.id = h.user_id WHERE h.tag_id = ? ORDER BY h.id DESC LIMIT 20`, tagID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hist []change
	for rows.Next() {
		var c change
		rows.Scan(&c.ID, &c.Old, &c.New, &c.Author, &c.At)
		hist = append(hist, c)
	}
	return hist, rows.Err()
}

func (a *app) business(id int64) (business, error) {
	var b business
	err := a.db.QueryRow(`SELECT id, name, plan, starts_at, expires_at, price FROM businesses WHERE id = ?`, id).Scan(&b.ID, &b.Name, &b.Plan, &b.StartsAt, &b.ExpiresAt, &b.Price)
	b.Status = subStatus(b.ExpiresAt, a.now())
	return b, err
}

// --- помощники ---

func (a *app) tx(fn func(*sql.Tx) error) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (a *app) kv(k string) string {
	var v string
	a.db.QueryRow(`SELECT v FROM kv WHERE k = ?`, k).Scan(&v)
	return v
}

func (a *app) setKV(k, v string) {
	if _, err := a.db.Exec(`INSERT INTO kv(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v); err != nil {
		log.Print(err)
	}
}

func (a *app) data(r *http.Request, s *session, title string, d map[string]any) map[string]any {
	d["Title"], d["User"], d["CSRF"], d["P"], d["Home"] = title, s.Email, s.CSRF, a.panel, a.home(s.Role)
	d["Path"] = r.URL.Path
	d["TG"] = map[string]bool{"Enabled": a.tg != nil, "Linked": s.TGLinked}
	if e := errMsgs[r.URL.Query().Get("err")]; e != "" {
		d["Error"] = e
	}
	if n := okMsgs[r.URL.Query().Get("ok")]; n != "" {
		d["Notice"] = n
	}
	return d
}

func (a *app) render(w http.ResponseWriter, code int, page string, d map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(code)
	if err := a.tpl[page].ExecuteTemplate(w, "layout", d); err != nil {
		log.Print(err)
	}
}

func (a *app) message(w http.ResponseWriter, code int, text string) {
	a.render(w, code, "message", map[string]any{"Title": text})
}

func (a *app) back(w http.ResponseWriter, r *http.Request, to, code string) {
	http.Redirect(w, r, to+"?err="+code, http.StatusSeeOther)
}

func (a *app) done(w http.ResponseWriter, r *http.Request, to, code string) {
	http.Redirect(w, r, to+"?ok="+code, http.StatusSeeOther)
}

func (a *app) notFound(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		a.message(w, 404, "Не найдено")
		return
	}
	a.fail(w, err)
}

func (a *app) fail(w http.ResponseWriter, err error) {
	log.Print(err)
	a.message(w, 500, "Ошибка сервера")
}

// subStatus: просрочено → «Требует продления», ≤30 дней → «Скоро закончится». Метки при этом не отключаются.
func subStatus(exp string, now time.Time) status {
	today := now.In(msk).Format(time.DateOnly)
	switch {
	case exp < today:
		return status{"badge-bad", "Требует продления"}
	case exp <= now.In(msk).AddDate(0, 0, 30).Format(time.DateOnly):
		return status{"badge-warn", "Скоро закончится"}
	}
	return status{"badge-ok", "Активен"}
}

// daysBetween — сколько дней от from до to (обе даты YYYY-MM-DD).
func daysBetween(from, to string) int {
	f, _ := time.Parse(time.DateOnly, from)
	t, _ := time.Parse(time.DateOnly, to)
	return int(t.Sub(f).Hours() / 24)
}

// plural: plural(5, "метка", "метки", "меток") → "5 меток".
func plural(n int, one, few, many string) string {
	w := many
	switch m10, m100 := n%10, n%100; {
	case m10 == 1 && m100 != 11:
		w = one
	case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
		w = few
	}
	return fmt.Sprintf("%d %s", n, w)
}

func ruDate(s string) string { // 2027-10-15 → 15.10.2027
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t.Format("02.01.2006")
	}
	return s
}

func validDate(s string) bool { _, err := time.Parse(time.DateOnly, s); return err == nil }

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && len(s) <= 2000 && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func validEmail(s string) bool { a, err := mail.ParseAddress(s); return err == nil && a.Address == s }

// newToken: 8 символов без похожих (0/O, 1/l/I) — ~46 бит, перебором не найти.
func newToken() string {
	const abc = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 8)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(abc))))
		b[i] = abc[n.Int64()]
	}
	return string(b)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const pbkdf2Iter = 600_000

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%x$%x", pbkdf2Iter, salt, key)
}

func checkPassword(stored, pw string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err1 := strconv.Atoi(p[1])
	salt, err2 := hex.DecodeString(p[2])
	want, err3 := hex.DecodeString(p[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}
