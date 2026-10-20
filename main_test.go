package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testEnv struct {
	t   *testing.T
	a   *app
	srv *httptest.Server
}

func newEnv(t *testing.T, c config) *testEnv {
	t.Helper()
	c.DBPath = filepath.Join(t.TempDir(), "t.db")
	if c.Public == "" {
		c.Public = "https://tap.example"
	}
	if c.Panel == "" {
		c.Panel = "/panel"
	}
	a, h, err := newApp(c)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &testEnv{t, a, srv}
}

// user — браузер одного пользователя: свои cookie, CSRF берём из его сессии в БД.
type user struct {
	e     *testEnv
	c     *http.Client
	email string
}

func (e *testEnv) user(email string) *user {
	jar, _ := cookiejar.New(nil)
	return &user{e, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, email}
}

func (u *user) post(path string, v url.Values) *http.Response {
	u.e.t.Helper()
	var csrf string
	u.e.a.db.QueryRow(`SELECT s.csrf FROM sessions s JOIN users us ON us.id = s.user_id WHERE us.email = ? ORDER BY s.rowid DESC LIMIT 1`, u.email).Scan(&csrf)
	v.Set("csrf", csrf)
	r, err := u.c.PostForm(u.e.srv.URL+path, v)
	if err != nil {
		u.e.t.Fatal(err)
	}
	r.Body.Close()
	return r
}

func (u *user) get(path string) (*http.Response, string) {
	u.e.t.Helper()
	r, err := u.c.Get(u.e.srv.URL + path)
	if err != nil {
		u.e.t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	return r, string(b)
}

func (e *testEnv) val(q string, args ...any) string {
	var s string
	e.a.db.QueryRow(q, args...).Scan(&s)
	return s
}

// seed: админ, предприятие с точкой и меткой. Возвращает админа и токен метки.
func (e *testEnv) seed() (*user, string) {
	admin := e.user("a@b.ru")
	if r := admin.post("/panel/setup", url.Values{"email": {"a@b.ru"}, "password": {"password123"}}); r.StatusCode != 303 {
		e.t.Fatalf("setup: %d", r.StatusCode)
	}
	admin.post("/panel/businesses", url.Values{"name": {"Barber House"}, "plan": {"Год"}, "price": {"1499"}, "expires_at": {"2099-01-01"}})
	admin.post("/panel/b/1/locations", url.Values{"name": {"Арбат"}, "address": {"ул. Арбат, 21"}})
	admin.post("/panel/l/1/tags", url.Values{"name": {"Касса"}, "url": {"https://yandex.ru/maps/old"}})
	return admin, e.val(`SELECT public_token FROM tags WHERE id = 1`)
}

// Полный сценарий из ТЗ: админ → предприятие → точка → метка → редирект → смена ссылки → откат.
func TestAdminFlow(t *testing.T) {
	e := newEnv(t, config{})
	admin, tok := e.seed()
	dest := func() string { return e.val(`SELECT destination_url FROM tags WHERE id = 1`) }

	e.user("evil@b.ru").post("/panel/setup", url.Values{"email": {"evil@b.ru"}, "password": {"password123"}})
	if n := e.val(`SELECT COUNT(*) FROM users`); n != "1" {
		t.Fatalf("second setup must not create user, got %s users", n)
	}
	if len(tok) != 8 {
		t.Fatalf("token %q", tok)
	}
	visitor := e.user("visitor")
	if r, _ := visitor.get("/t/" + tok); r.StatusCode != 302 || r.Header.Get("Location") != "https://yandex.ru/maps/old" {
		t.Fatalf("redirect: %d %s", r.StatusCode, r.Header.Get("Location"))
	}

	admin.post("/panel/g/1", url.Values{"name": {"Касса"}, "url": {"javascript:alert(1)"}, "active": {"1"}})
	if dest() != "https://yandex.ru/maps/old" {
		t.Fatal("bad url accepted")
	}
	admin.post("/panel/g/1", url.Values{"name": {"Касса"}, "url": {"https://yandex.ru/maps/new"}, "active": {"1"}})
	if r, _ := visitor.get("/t/" + tok); r.Header.Get("Location") != "https://yandex.ru/maps/new" {
		t.Fatalf("after edit: %s", r.Header.Get("Location"))
	}
	if e.val(`SELECT user_id FROM tag_history WHERE id = 1`) != "1" {
		t.Fatal("history author")
	}
	admin.post("/panel/g/1/revert/1", url.Values{})
	if dest() != "https://yandex.ru/maps/old" {
		t.Fatalf("revert: %s", dest())
	}

	admin.post("/panel/g/1", url.Values{"name": {"Касса"}, "url": {"https://yandex.ru/maps/old"}, "active": {"0"}})
	if r, _ := visitor.get("/t/" + tok); r.StatusCode != 403 {
		t.Fatalf("disabled tag: %d", r.StatusCode)
	}
	if r, _ := visitor.get("/t/nope1234"); r.StatusCode != 404 {
		t.Fatalf("unknown tag: %d", r.StatusCode)
	}
	if n := e.val(`SELECT COUNT(*) FROM tag_clicks`); n != "2" {
		t.Fatalf("clicks %s", n)
	}

	for _, p := range []string{"/panel/", "/panel/b/1", "/panel/b/1?days=7", "/panel/l/1", "/panel/g/1", "/panel/g/1?days=90"} {
		if r, body := admin.get(p); r.StatusCode != 200 || strings.Contains(body, "ZgotmplZ") {
			t.Fatalf("GET %s: %d", p, r.StatusCode)
		}
	}

	r, _ := admin.c.PostForm(e.srv.URL+"/panel/g/1", url.Values{"name": {"x"}, "url": {"https://evil.example"}, "active": {"1"}})
	if r.StatusCode != 403 || dest() == "https://evil.example" {
		t.Fatalf("csrf: %d", r.StatusCode)
	}

	// ручная оплата продлевает на год от даты окончания
	admin.post("/panel/b/1/paid", url.Values{"amount": {"1499"}})
	if exp := e.val(`SELECT expires_at FROM businesses WHERE id = 1`); exp != "2100-01-01" {
		t.Fatalf("manual payment: expires %s", exp)
	}

	admin.post("/panel/logout", url.Values{})
	if r, _ := admin.get("/panel/g/1"); r.StatusCode != 303 {
		t.Fatalf("after logout: %d", r.StatusCode)
	}
}

func TestBotsNotCounted(t *testing.T) {
	e := newEnv(t, config{})
	_, tok := e.seed()
	for _, ua := range []string{"TelegramBot (like TwitterBot)", "WhatsApp/2.23", "Mozilla/5.0 (compatible; YandexBot/3.0)"} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/t/"+tok, nil)
		req.Header.Set("User-Agent", ua)
		r, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil || r.StatusCode != 302 {
			t.Fatalf("%s: redirect must still work", ua)
		}
	}
	if n := e.val(`SELECT COUNT(*) FROM tag_clicks`); n != "0" {
		t.Fatalf("bots counted: %s", n)
	}
}

func TestCabinet(t *testing.T) {
	e := newEnv(t, config{})
	admin, _ := e.seed()
	// второе предприятие — чужое для клиента
	admin.post("/panel/businesses", url.Values{"name": {"Чужая кофейня"}, "price": {"1499"}, "expires_at": {"2099-01-01"}})
	admin.post("/panel/b/2/locations", url.Values{"name": {"Тверская"}})
	admin.post("/panel/l/2/tags", url.Values{"name": {"Чужая"}, "url": {"https://other.example"}})

	if r := admin.post("/panel/b/1/clients", url.Values{"email": {"owner@barber.ru"}, "password": {"short"}}); !strings.Contains(r.Header.Get("Location"), "err=pw") {
		t.Fatal("short password accepted")
	}
	admin.post("/panel/b/1/clients", url.Values{"email": {"owner@barber.ru"}, "password": {"clientpass1"}})
	if r := admin.post("/panel/b/2/clients", url.Values{"email": {"owner@barber.ru"}, "password": {"clientpass1"}}); !strings.Contains(r.Header.Get("Location"), "err=exists") {
		t.Fatal("duplicate email accepted")
	}

	c := e.user("owner@barber.ru")
	if r := c.post("/cabinet/login", url.Values{"email": {"owner@barber.ru"}, "password": {"clientpass1"}}); r.Header.Get("Location") != "/cabinet/" {
		t.Fatalf("client login -> %s", r.Header.Get("Location"))
	}
	if r, body := c.get("/cabinet/"); r.StatusCode != 200 || !strings.Contains(body, "Barber House") || strings.Contains(body, "Чужая") {
		t.Fatalf("cabinet: %d", r.StatusCode)
	}
	if r, _ := c.get("/cabinet/g/2"); r.StatusCode != 404 {
		t.Fatalf("foreign tag visible: %d", r.StatusCode)
	}
	// клиент только смотрит: менять ссылки (свои и чужие) не может
	for _, id := range []string{"1", "2"} {
		if r := c.post("/cabinet/g/"+id, url.Values{"url": {"https://hacked.example"}, "active": {"1"}}); r.StatusCode != 405 {
			t.Fatalf("client tag update allowed: %d", r.StatusCode)
		}
	}
	if e.val(`SELECT COUNT(*) FROM tags WHERE destination_url = 'https://hacked.example'`) != "0" {
		t.Fatal("client changed tag")
	}
	if r, body := c.get("/cabinet/g/1"); r.StatusCode != 200 || strings.Contains(body, `name="url"`) {
		t.Fatalf("client tag page must be read-only: %d", r.StatusCode)
	}
	// клиент в админку не попадает
	if r, _ := c.get("/panel/b/1"); r.StatusCode != 303 {
		t.Fatalf("client sees admin page: %d", r.StatusCode)
	}
	if r := c.post("/panel/businesses", url.Values{"name": {"x"}, "price": {"1"}, "expires_at": {"2099-01-01"}}); r.StatusCode != 403 {
		t.Fatalf("client admin action: %d", r.StatusCode)
	}
	// смена пароля
	c.post("/cabinet/password", url.Values{"old": {"clientpass1"}, "new": {"newpass123"}})
	if !checkPassword(e.val(`SELECT password_hash FROM users WHERE email = 'owner@barber.ru'`), "newpass123") {
		t.Fatal("password not changed")
	}
	// удаление доступа закрывает сессии
	admin.post("/panel/users/2/delete", url.Values{})
	if r, _ := c.get("/cabinet/g/1"); r.StatusCode != 303 {
		t.Fatalf("deleted client still in: %d", r.StatusCode)
	}
}

// fakeYK — подставная ЮKassa: создаёт платёж, статус меняет тест.
type fakeYK struct {
	mu      sync.Mutex
	status  string
	created int
}

func (f *fakeYK) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, _ := r.BasicAuth(); u != "shop" || p != "secret" {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/payments":
		if r.Header.Get("Idempotence-Key") == "" {
			http.Error(w, "no idempotence key", 400)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["amount"].(map[string]any)["value"] != "1499.00" {
			http.Error(w, "bad amount", 400)
			return
		}
		f.created++
		json.NewEncoder(w).Encode(map[string]any{"id": "yk1", "status": "pending", "confirmation": map[string]string{"confirmation_url": "https://pay.example/yk1"}})
	case r.Method == "GET" && r.URL.Path == "/payments/yk1":
		json.NewEncoder(w).Encode(map[string]any{"id": "yk1", "status": f.status})
	default:
		http.NotFound(w, r)
	}
}

func TestYooKassa(t *testing.T) {
	yk := &fakeYK{status: "pending"}
	ykSrv := httptest.NewServer(yk)
	defer ykSrv.Close()
	e := newEnv(t, config{YK: ykConfig{ShopID: "shop", Secret: "secret", API: ykSrv.URL}})
	admin, _ := e.seed()
	e.a.db.Exec(`UPDATE users SET tg_chat_id = 100 WHERE id = 1`)
	today := time.Now().In(msk).Format(time.DateOnly)
	e.a.db.Exec(`UPDATE businesses SET expires_at = '2020-01-01' WHERE id = 1`) // истекла давно
	admin.post("/panel/b/1/clients", url.Values{"email": {"owner@barber.ru"}, "password": {"clientpass1"}})
	c := e.user("owner@barber.ru")
	c.post("/cabinet/login", url.Values{"email": {"owner@barber.ru"}, "password": {"clientpass1"}})

	if r := c.post("/cabinet/pay", url.Values{}); r.Header.Get("Location") != "https://pay.example/yk1" {
		t.Fatalf("pay redirect: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if e.val(`SELECT status || ':' || external_id FROM payments`) != "pending:yk1" {
		t.Fatal("payment not saved")
	}
	webhook := func() int {
		r, err := http.Post(e.srv.URL+"/pay/yookassa", "application/json", strings.NewReader(`{"event":"payment.succeeded","object":{"id":"yk1","status":"succeeded"}}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	// подделка: уведомление пришло, а в ЮKassa платёж ещё не оплачен — не верим
	if webhook() != 200 || e.val(`SELECT status FROM payments`) != "pending" {
		t.Fatal("forged webhook accepted")
	}
	yk.status = "succeeded"
	webhook()
	webhook() // повтор уведомления не продлевает второй раз
	want := extendYear("2020-01-01", today)
	if exp := e.val(`SELECT expires_at FROM businesses WHERE id = 1`); exp != want {
		t.Fatalf("expires %s, want %s", exp, want)
	}
	if e.val(`SELECT starts_at FROM businesses WHERE id = 1`) != today {
		t.Fatal("starts_at not reset for expired subscription")
	}
	if n := e.val(`SELECT COUNT(*) FROM outbox WHERE chat_id = 100 AND text LIKE '%Оплата 1499%'`); n != "1" {
		t.Fatalf("admin payment notifications: %s", n)
	}
	if r, body := c.get("/cabinet/pay/1"); r.StatusCode != 200 || !strings.Contains(body, "Оплата прошла") {
		t.Fatalf("return page: %d", r.StatusCode)
	}
	if extendYear("2030-05-10", "2026-10-19") != "2031-05-10" {
		t.Fatal("extend from future expiry")
	}
}

// fakeTG — подставной Bot API, запоминает отправленные сообщения.
type fakeTG struct {
	mu   sync.Mutex
	sent []map[string]any
	down bool
}

func (f *fakeTG) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "blocked", 502)
		return
	}
	var p map[string]any
	json.NewDecoder(r.Body).Decode(&p)
	switch {
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		f.sent = append(f.sent, p)
		w.Write([]byte(`{"ok":true,"result":{}}`))
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		w.Write([]byte(`{"ok":true,"result":{"username":"nfc_test_bot"}}`))
	default:
		w.Write([]byte(`{"ok":false,"error_code":404,"description":"not found"}`))
	}
}

func TestTelegram(t *testing.T) {
	tg := &fakeTG{}
	tgSrv := httptest.NewServer(tg)
	defer tgSrv.Close()
	e := newEnv(t, config{TGToken: "123:abc", TGAPI: tgSrv.URL})
	admin, _ := e.seed()
	admin.post("/panel/b/1/clients", url.Values{"email": {"owner@barber.ru"}, "password": {"clientpass1"}})

	// привязка через /start <код>
	e.a.tg.username = "nfc_test_bot"
	r := admin.post("/panel/telegram", url.Values{})
	code := strings.TrimPrefix(r.Header.Get("Location"), "https://t.me/nfc_test_bot?start=")
	if code == "" || code == r.Header.Get("Location") {
		t.Fatalf("link: %s", r.Header.Get("Location"))
	}
	e.a.tgHandle(100, "/start "+code)
	e.a.tgHandle(999, "/start "+code) // код одноразовый
	if e.val(`SELECT tg_chat_id FROM users WHERE id = 1`) != "100" {
		t.Fatal("admin not linked")
	}
	e.a.db.Exec(`UPDATE users SET tg_chat_id = 200 WHERE email = 'owner@barber.ru'`)

	// понедельник 19.10.2026 11:00 МСК; подписка истекает через 7 дней; прошлая неделя — 3 перехода
	now := time.Date(2026, 10, 19, 11, 0, 0, 0, msk)
	e.a.now = func() time.Time { return now }
	e.a.db.Exec(`UPDATE businesses SET expires_at = '2026-10-26'`)
	for _, d := range []int{-1, -2, -3} {
		e.a.db.Exec(`INSERT INTO tag_clicks(tag_id, created_at) VALUES(1, ?)`, now.AddDate(0, 0, d).Unix())
	}

	e.a.daily()
	e.a.setKV("daily", "") // даже если флаг дня потерян, dedupe не даст дублей
	e.a.daily()
	count := func(chat int, like string) string {
		return e.val(`SELECT COUNT(*) FROM outbox WHERE chat_id = ? AND text LIKE ?`, chat, like)
	}
	if count(100, "%Barber House — истекает 26.10.2026 (через 7 дн.)%") != "1" {
		t.Fatal("admin digest")
	}
	if count(200, "%Обслуживание меток mymark для «Barber House» истекает 26.10.2026%") != "1" {
		t.Fatal("client reminder")
	}
	if count(200, "%Всего: 3%Касса (Арбат) — 3%") != "1" {
		t.Fatal("weekly report")
	}

	// Telegram недоступен — сообщения остаются в очереди
	tg.down = true
	e.a.flushOutbox(t.Context())
	if e.val(`SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL`) == "0" {
		t.Fatal("messages lost while telegram down")
	}
	tg.down = false
	e.a.db.Exec(`UPDATE outbox SET next_at = 0`)
	for range 3 {
		e.a.flushOutbox(t.Context())
	}
	if n := e.val(`SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL`); n != "0" {
		t.Fatalf("unsent: %s", n)
	}
	if len(tg.sent) != 5 { // привязка-ответ, «код не найден», дайджест, напоминание, отчёт
		t.Fatalf("sent %d", len(tg.sent))
	}
}

func TestStats(t *testing.T) {
	e := newEnv(t, config{})
	e.seed()
	now := time.Date(2026, 10, 19, 15, 30, 0, 0, msk) // понедельник
	e.a.now = func() time.Time { return now }
	add := func(at time.Time, device, visitor string) {
		e.a.db.Exec(`INSERT INTO tag_clicks(tag_id, created_at, device, visitor) VALUES(1, ?, ?, ?)`, at.Unix(), device, visitor)
	}
	add(now, "ios", "v1")
	add(now.Add(-time.Hour), "ios", "v1")
	add(now.AddDate(0, 0, -1), "android", "v2")              // воскресенье 15:30
	add(time.Date(2026, 10, 19, 0, 30, 0, 0, msk), "", "v3") // 00:30 МСК — ещё «сегодня» по Москве
	add(now.AddDate(0, 0, -40), "ios", "v4")                 // вне 30 дней

	st, err := e.a.stats("tag", 1, 30)
	if err != nil {
		t.Fatal(err)
	}
	if st.Period != 4 || st.AllTime != 5 || st.Today != 3 || st.Unique != 3 {
		t.Fatalf("period %d all %d today %d unique %d", st.Period, st.AllTime, st.Today, st.Unique)
	}
	if len(st.Daily) != 30 || st.Daily[29].Value != 3 || st.Daily[28].Value != 1 {
		t.Fatalf("daily %+v", st.Daily[28:])
	}
	if st.Hours[15].Value != 2 || st.Hours[14].Value != 1 || st.Hours[0].Value != 1 {
		t.Fatal("hours (MSK)")
	}
	if st.Weekdays[0].Value != 3 || st.Weekdays[6].Value != 1 {
		t.Fatalf("weekdays %+v", st.Weekdays)
	}
	if st.Devices[0].Value != 2 || st.Devices[1].Value != 1 || st.Devices[2].Value != 1 {
		t.Fatalf("devices %+v", st.Devices)
	}
	bs, _ := e.a.stats("business", 1, 7)
	if len(bs.Top) != 1 || bs.Top[0].Clicks != 4 || len(bs.Daily) != 7 {
		t.Fatalf("business stats %+v", bs.Top)
	}
}

func TestSubStatus(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, msk)
	for exp, want := range map[string]string{"2026-10-18": "badge-bad", "2026-10-19": "badge-warn", "2026-11-18": "badge-warn", "2026-11-19": "badge-ok"} {
		if got := subStatus(exp, now).Class; got != want {
			t.Errorf("%s: %s, want %s", exp, got, want)
		}
	}
}

func TestPassword(t *testing.T) {
	h := hashPassword("secret123")
	if !checkPassword(h, "secret123") || checkPassword(h, "secret124") || strings.Contains(h, "secret") {
		t.Fatal("password hashing broken")
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{1: "1 метка", 2: "2 метки", 5: "5 меток", 11: "11 меток", 12: "12 меток", 21: "21 метка", 22: "22 метки", 0: "0 меток"} {
		if got := plural(n, "метка", "метки", "меток"); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}
