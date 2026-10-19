package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Оплата через ЮKassa: https://yookassa.ru/developers/api
// Клиент жмёт «Продлить» → создаём платёж → редирект на страницу ЮKassa → после оплаты
// ЮKassa шлёт уведомление на /pay/yookassa и возвращает клиента на /cabinet/pay/{id}.
// Статус всегда перечитываем из API ЮKassa — телу уведомления не доверяем.

type ykConfig struct {
	ShopID, Secret, API string
	Receipt             bool // передавать чек (54-ФЗ), если в магазине включена онлайн-касса
}

func (y ykConfig) enabled() bool { return y.ShopID != "" && y.Secret != "" }

type ykPayment struct {
	ID           string `json:"id"`
	Status       string `json:"status"` // pending | waiting_for_capture | succeeded | canceled
	Confirmation struct {
		URL string `json:"confirmation_url"`
	} `json:"confirmation"`
}

func (a *app) ykCall(ctx context.Context, method, path, idem string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.yk.API+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(a.yk.ShopID, a.yk.Secret)
	req.Header.Set("Content-Type", "application/json")
	if idem != "" {
		req.Header.Set("Idempotence-Key", idem)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("yookassa %s %s: %d %s", method, path, resp.StatusCode, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (a *app) startPayment(w http.ResponseWriter, r *http.Request, s *session) {
	if !a.yk.enabled() {
		a.back(w, r, "/cabinet/", "nopay")
		return
	}
	b, err := a.business(s.BusinessID)
	if err != nil {
		a.notFound(w, err)
		return
	}
	res, err := a.db.Exec(`INSERT INTO payments(business_id, user_id, provider, amount, status, created_at) VALUES(?, ?, 'yookassa', ?, 'pending', ?)`, b.ID, s.UserID, b.Price, a.now().Unix())
	if err != nil {
		a.fail(w, err)
		return
	}
	pid, _ := res.LastInsertId()

	desc := []rune("Обслуживание NFC-меток на 12 месяцев: " + b.Name)
	if len(desc) > 128 { // лимит ЮKassa
		desc = desc[:128]
	}
	amount := map[string]string{"value": fmt.Sprintf("%d.00", b.Price), "currency": "RUB"}
	body := map[string]any{
		"amount":       amount,
		"capture":      true,
		"description":  string(desc),
		"confirmation": map[string]string{"type": "redirect", "return_url": fmt.Sprintf("%s/cabinet/pay/%d", a.public, pid)},
		"metadata":     map[string]string{"payment_id": strconv.FormatInt(pid, 10)},
	}
	if a.yk.Receipt {
		body["receipt"] = map[string]any{
			"customer": map[string]string{"email": s.Email},
			"items": []any{map[string]any{
				"description": string(desc), "quantity": "1.00", "amount": amount,
				"vat_code": 1, "payment_subject": "service", "payment_mode": "full_payment",
			}},
		}
	}
	var p ykPayment
	if err := a.ykCall(r.Context(), http.MethodPost, "/payments", fmt.Sprintf("nfc-payment-%d", pid), body, &p); err != nil || p.Confirmation.URL == "" {
		log.Printf("yookassa create payment %d: %v", pid, err)
		a.db.Exec(`UPDATE payments SET status = 'error' WHERE id = ?`, pid)
		a.back(w, r, "/cabinet/", "payfail")
		return
	}
	if _, err := a.db.Exec(`UPDATE payments SET external_id = ? WHERE id = ?`, p.ID, pid); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, p.Confirmation.URL, http.StatusSeeOther)
}

// syncPayment перечитывает платёж из ЮKassa и применяет статус. Повторные вызовы безопасны.
func (a *app) syncPayment(ctx context.Context, extID string) error {
	var p ykPayment
	if err := a.ykCall(ctx, http.MethodGet, "/payments/"+url.PathEscape(extID), "", nil, &p); err != nil {
		return err
	}
	var pid int64
	err := a.db.QueryRow(`SELECT id FROM payments WHERE external_id = ?`, p.ID).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // не наш платёж
	}
	if err != nil {
		return err
	}
	switch p.Status {
	case "succeeded":
		return a.markPaid(pid, true)
	case "canceled":
		_, err := a.db.Exec(`UPDATE payments SET status = 'canceled' WHERE id = ? AND status = 'pending'`, pid)
		return err
	}
	return nil
}

// markPaid отмечает платёж оплаченным и продлевает подписку на год — ровно один раз.
func (a *app) markPaid(pid int64, notify bool) error {
	var name, newExp string
	var amount int
	var done bool
	err := a.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE payments SET status = 'succeeded', paid_at = ? WHERE id = ? AND status = 'pending'`, a.now().Unix(), pid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // уже обработан
		}
		var biz int64
		var exp string
		if err := tx.QueryRow(`SELECT p.business_id, p.amount, b.name, b.expires_at FROM payments p JOIN businesses b ON b.id = p.business_id WHERE p.id = ?`, pid).
			Scan(&biz, &amount, &name, &exp); err != nil {
			return err
		}
		today := a.now().In(msk).Format(time.DateOnly)
		newExp = extendYear(exp, today)
		_, err = tx.Exec(`UPDATE businesses SET starts_at = CASE WHEN expires_at < ? THEN ? ELSE starts_at END, expires_at = ? WHERE id = ?`, today, today, newExp, biz)
		done = err == nil
		return err
	})
	if err == nil && done && notify {
		a.notifyAdmins(fmt.Sprintf("💰 Оплата %d ₽ — %s\nПодписка продлена до %s", amount, name, ruDate(newExp)), fmt.Sprintf("paid:%d", pid))
	}
	return err
}

// extendYear: продление считается от даты окончания, а если она уже прошла — от сегодня.
func extendYear(exp, today string) string {
	base := exp
	if base < today {
		base = today
	}
	t, _ := time.Parse(time.DateOnly, base)
	return t.AddDate(1, 0, 0).Format(time.DateOnly)
}

func (a *app) ykWebhook(w http.ResponseWriter, r *http.Request) {
	if !a.yk.enabled() {
		http.NotFound(w, r)
		return
	}
	var n struct {
		Event  string `json:"event"`
		Object struct {
			ID string `json:"id"`
		} `json:"object"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&n); err != nil || n.Object.ID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := a.syncPayment(r.Context(), n.Object.ID); err != nil {
		log.Printf("yookassa webhook %s: %v", n.Object.ID, err)
		http.Error(w, "retry later", http.StatusInternalServerError) // ЮKassa повторит уведомление
		return
	}
	w.WriteHeader(http.StatusOK)
}

// paymentReturn — сюда ЮKassa возвращает клиента. Проверяем статус сами, не дожидаясь уведомления.
func (a *app) paymentReturn(w http.ResponseWriter, r *http.Request, s *session) {
	var status string
	var ext sql.NullString
	q := `SELECT status, external_id FROM payments WHERE id = ? AND business_id = ?`
	if err := a.db.QueryRow(q, r.PathValue("id"), s.BusinessID).Scan(&status, &ext); err != nil {
		a.notFound(w, err)
		return
	}
	if status == "pending" && ext.Valid {
		if err := a.syncPayment(r.Context(), ext.String); err != nil {
			log.Print(err)
		}
		a.db.QueryRow(q, r.PathValue("id"), s.BusinessID).Scan(&status, &ext)
	}
	b, _ := a.business(s.BusinessID)
	a.render(w, 200, "payment", a.data(r, s, "Оплата", map[string]any{"Status": status, "B": b}))
}
