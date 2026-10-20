package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Telegram-уведомления. Все сообщения сначала пишутся в таблицу outbox, отдельный цикл отправляет
// их с повторами — если api.telegram.org недоступен (в РФ бывает), ничего не теряется.
// Обход блокировки: HTTPS_PROXY=socks5://... (Go подхватывает сам) или TELEGRAM_API=свой Bot API.

type telegram struct {
	base string // https://api.telegram.org/bot<token>
	http *http.Client

	mu       sync.Mutex
	username string
}

type tgError struct {
	Code int
	Desc string
}

func (e *tgError) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Desc) }

func (t *telegram) call(ctx context.Context, method string, params, out any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) { // в URL токен бота — в лог его не пишем
			return fmt.Errorf("telegram %s: %v", method, ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	var r struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Desc   string          `json:"description"`
		Code   int             `json:"error_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: %d %v", method, resp.StatusCode, err)
	}
	if !r.OK {
		return &tgError{r.Code, r.Desc}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (a *app) botName() string {
	if a.tg == nil {
		return ""
	}
	a.tg.mu.Lock()
	defer a.tg.mu.Unlock()
	return a.tg.username
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// tgPoll получает сообщения боту (long polling — публичный адрес и вебхук не нужны).
func (a *app) tgPoll(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		if a.botName() == "" {
			var me struct {
				Username string `json:"username"`
			}
			if err := a.tg.call(ctx, "getMe", map[string]any{}, &me); err != nil {
				log.Printf("telegram недоступен: %v", err)
				sleep(ctx, time.Minute)
				continue
			}
			a.tg.mu.Lock()
			a.tg.username = me.Username
			a.tg.mu.Unlock()
		}
		var ups []struct {
			ID  int64 `json:"update_id"`
			Msg *struct {
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
				Text string `json:"text"`
			} `json:"message"`
		}
		err := a.tg.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message"}}, &ups)
		if err != nil {
			log.Printf("telegram getUpdates: %v", err)
			sleep(ctx, 15*time.Second)
			continue
		}
		for _, u := range ups {
			offset = u.ID + 1
			if u.Msg != nil {
				a.tgHandle(u.Msg.Chat.ID, u.Msg.Text)
			}
		}
	}
}

// tgHandle: /start <код> из ссылки «Привязать Telegram» связывает чат с пользователем.
func (a *app) tgHandle(chat int64, text string) {
	if code, ok := strings.CutPrefix(strings.TrimSpace(text), "/start "); ok && strings.TrimSpace(code) != "" {
		res, err := a.db.Exec(`UPDATE users SET tg_chat_id = ?, tg_code = NULL WHERE tg_code = ?`, chat, strings.TrimSpace(code))
		if err != nil {
			log.Print(err)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			a.enqueue(chat, "✅ Готово! Сюда будут приходить уведомления mymark.", "")
			return
		}
	}
	a.enqueue(chat, "Чтобы получать уведомления, нажмите «Подключить» в панели mymark.", "")
}

func (a *app) tgLink(w http.ResponseWriter, r *http.Request, s *session) {
	name := a.botName()
	if name == "" {
		a.back(w, r, a.home(s.Role), "tg")
		return
	}
	code := randHex(12)
	if _, err := a.db.Exec(`UPDATE users SET tg_code = ? WHERE id = ?`, code, s.UserID); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "https://t.me/"+name+"?start="+code, http.StatusSeeOther)
}

func (a *app) tgUnlink(w http.ResponseWriter, r *http.Request, s *session) {
	if _, err := a.db.Exec(`UPDATE users SET tg_chat_id = NULL, tg_code = NULL WHERE id = ?`, s.UserID); err != nil {
		a.fail(w, err)
		return
	}
	a.done(w, r, a.home(s.Role), "tgoff")
}

// enqueue ставит сообщение в очередь. dedupe != "" — повторная постановка с тем же ключом игнорируется.
func (a *app) enqueue(chat int64, text, dedupe string) {
	var d any
	if dedupe != "" {
		d = dedupe
	}
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO outbox(chat_id, text, dedupe, created_at) VALUES(?, ?, ?, ?)`, chat, text, d, a.now().Unix()); err != nil {
		log.Print(err)
	}
}

func (a *app) notifyUsers(where string, arg any, text, dedupe string) {
	rows, err := a.db.Query(`SELECT id, tg_chat_id FROM users WHERE tg_chat_id IS NOT NULL AND `+where, arg)
	if err != nil {
		log.Print(err)
		return
	}
	type to struct{ id, chat int64 }
	var list []to
	for rows.Next() {
		var t to
		rows.Scan(&t.id, &t.chat)
		list = append(list, t)
	}
	rows.Close()
	for _, t := range list {
		key := ""
		if dedupe != "" {
			key = fmt.Sprintf("%s:u%d", dedupe, t.id)
		}
		a.enqueue(t.chat, text, key)
	}
}

func (a *app) notifyAdmins(text, dedupe string) { a.notifyUsers("role = ?", "admin", text, dedupe) }

func (a *app) notifyClients(biz int64, text, dedupe string) {
	a.notifyUsers("role = 'client' AND business_id = ?", biz, text, dedupe)
}

func (a *app) tgSender(ctx context.Context) {
	for ctx.Err() == nil {
		a.flushOutbox(ctx)
		sleep(ctx, 10*time.Second)
	}
}

func (a *app) flushOutbox(ctx context.Context) {
	now := a.now().Unix()
	rows, err := a.db.Query(`SELECT id, chat_id, text, attempts FROM outbox WHERE sent_at IS NULL AND attempts < 10 AND next_at <= ? ORDER BY id LIMIT 30`, now)
	if err != nil {
		log.Print(err)
		return
	}
	type msg struct {
		id, chat int64
		text     string
		attempts int
	}
	var list []msg
	for rows.Next() {
		var m msg
		rows.Scan(&m.id, &m.chat, &m.text, &m.attempts)
		list = append(list, m)
	}
	rows.Close()
	for _, m := range list {
		err := a.tg.call(ctx, "sendMessage", map[string]any{"chat_id": m.chat, "text": m.text, "link_preview_options": map[string]bool{"is_disabled": true}}, nil)
		if err == nil {
			a.db.Exec(`UPDATE outbox SET sent_at = ?, error = '' WHERE id = ?`, a.now().Unix(), m.id)
			continue
		}
		attempts := m.attempts + 1
		var te *tgError
		if errors.As(err, &te) && (te.Code == 400 || te.Code == 403) {
			attempts = 99 // бот заблокирован или чат не существует — повторять бессмысленно
		}
		backoff := min(30*time.Second<<attempts, 6*time.Hour)
		a.db.Exec(`UPDATE outbox SET attempts = ?, next_at = ?, error = ? WHERE id = ?`, attempts, a.now().Add(backoff).Unix(), err.Error(), m.id)
		if attempts < 99 {
			return // сеть/Telegram недоступны — остальное попробуем в следующий раз
		}
	}
}

// --- ежедневные задачи ---

func (a *app) scheduler(ctx context.Context) {
	for ctx.Err() == nil {
		a.daily()
		sleep(ctx, 5*time.Minute)
	}
}

// daily раз в день после 10:00 МСК: напоминания о подписках, по понедельникам — недельные отчёты.
// Ключи dedupe в outbox защищают от повторов даже при перезапусках.
func (a *app) daily() {
	now := a.now().In(msk)
	if now.Hour() < 10 {
		return
	}
	day := now.Format(time.DateOnly)
	if a.kv("daily") == day {
		return
	}
	a.setKV("daily", day)
	a.expiryReminders(now)
	if now.Weekday() == time.Monday {
		a.weeklyReports(now)
	}
}

func (a *app) expiryReminders(now time.Time) {
	today := now.Format(time.DateOnly)
	rows, err := a.db.Query(`SELECT id, name, expires_at FROM businesses ORDER BY expires_at`)
	if err != nil {
		log.Print(err)
		return
	}
	type biz struct {
		id        int64
		name, exp string
	}
	var list []biz
	for rows.Next() {
		var b biz
		rows.Scan(&b.id, &b.name, &b.exp)
		list = append(list, b)
	}
	rows.Close()

	var lines []string
	for _, b := range list {
		d := daysBetween(today, b.exp)
		if !(d == 30 || d == 7 || d == 3 || d == 1 || d == 0 || (d < 0 && -d%7 == 0)) {
			continue
		}
		var when string
		switch {
		case d > 0:
			when = fmt.Sprintf("истекает %s (через %d дн.)", ruDate(b.exp), d)
		case d == 0:
			when = "истекает сегодня"
		default:
			when = fmt.Sprintf("истекла %s (%d дн. назад)", ruDate(b.exp), -d)
		}
		lines = append(lines, "• "+b.name+" — "+when)

		if d >= 0 || d == -7 { // клиенту: до окончания и одно напоминание через неделю после
			text := fmt.Sprintf("Обслуживание меток mymark для «%s» %s. Чтобы продлить, свяжитесь с нами.", b.name, when)
			if d < 0 {
				text = fmt.Sprintf("Обслуживание меток mymark для «%s» %s. Метки пока работают — чтобы продлить, свяжитесь с нами.", b.name, when)
			}
			a.notifyClients(b.id, text, fmt.Sprintf("remind:%d:%s:%d", b.id, b.exp, d))
		}
	}
	if len(lines) > 0 {
		a.notifyAdmins("⏰ Подписки, требующие внимания:\n"+strings.Join(lines, "\n")+"\n\n"+a.public+a.panel+"/", "digest:"+today)
	}
}

func (a *app) weeklyReports(now time.Time) {
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, msk) // понедельник 00:00
	start, prev := end.AddDate(0, 0, -7), end.AddDate(0, 0, -14)
	rows, err := a.db.Query(`SELECT DISTINCT b.id, b.name FROM businesses b JOIN users u ON u.business_id = b.id WHERE u.role = 'client' AND u.tg_chat_id IS NOT NULL`)
	if err != nil {
		log.Print(err)
		return
	}
	type biz struct {
		id   int64
		name string
	}
	var list []biz
	for rows.Next() {
		var b biz
		rows.Scan(&b.id, &b.name)
		list = append(list, b)
	}
	rows.Close()

	const scope = `c.tag_id IN (SELECT t.id FROM tags t JOIN locations l ON l.id = t.location_id WHERE l.business_id = ?)`
	for _, b := range list {
		var cur, before int
		a.db.QueryRow(`SELECT COUNT(CASE WHEN c.created_at >= ? THEN 1 END), COUNT(CASE WHEN c.created_at < ? THEN 1 END) FROM tag_clicks c WHERE c.created_at >= ? AND c.created_at < ? AND `+scope,
			start.Unix(), start.Unix(), prev.Unix(), end.Unix(), b.id).Scan(&cur, &before)
		text := fmt.Sprintf("📊 %s: переходы за неделю %s–%s\nВсего: %d (%s к прошлой неделе)", b.name, start.Format("02.01"), end.AddDate(0, 0, -1).Format("02.01"), cur, delta(cur, before))
		top, err := a.db.Query(`SELECT t.name, l.name, COUNT(*) FROM tag_clicks c JOIN tags t ON t.id = c.tag_id JOIN locations l ON l.id = t.location_id
			WHERE c.created_at >= ? AND c.created_at < ? AND l.business_id = ? GROUP BY t.id ORDER BY 3 DESC LIMIT 3`, start.Unix(), end.Unix(), b.id)
		if err == nil {
			for top.Next() {
				var tn, ln string
				var n int
				top.Scan(&tn, &ln, &n)
				text += fmt.Sprintf("\n• %s (%s) — %d", tn, ln, n)
			}
			top.Close()
		}
		text += "\n\nПодробнее: " + a.public + "/cabinet/"
		a.notifyClients(b.id, text, fmt.Sprintf("weekly:%d:%s", b.id, start.Format(time.DateOnly)))
	}
}

func delta(cur, prev int) string {
	if prev == 0 {
		if cur == 0 {
			return "без изменений"
		}
		return "раньше не было"
	}
	p := (cur - prev) * 100 / prev
	if p >= 0 {
		return fmt.Sprintf("+%d%%", p)
	}
	return fmt.Sprintf("−%d%%", -p)
}
