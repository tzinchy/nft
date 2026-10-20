package main

import (
	"net/http"
)

// Кабинет клиента — только просмотр своего предприятия: точки, метки, статистика, срок обслуживания.
// Настраивать в нём нечего: всё меняет админ. Маршруты оплаты, Telegram и смены пароля остаются
// на сервере, но в интерфейсе кабинета не показаны.

func (a *app) cabinet(w http.ResponseWriter, r *http.Request) {
	s := a.session(r)
	if s == nil {
		a.authPage(w, 200, false, "/cabinet/login", "")
		return
	}
	if s.Role != "client" || s.BusinessID == 0 {
		http.Redirect(w, r, a.home(s.Role), http.StatusSeeOther)
		return
	}
	b, err := a.business(s.BusinessID)
	if err != nil {
		a.notFound(w, err)
		return
	}
	locs, err := a.locations(b.ID, true)
	if err != nil {
		a.fail(w, err)
		return
	}
	st, err := a.stats("business", b.ID, periodDays(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	tagCount := 0
	for _, l := range locs {
		tagCount += len(l.TagList)
	}
	a.render(w, 200, "cabinet", a.data(r, s, b.Name, map[string]any{
		"B": b, "Locations": locs, "TagCount": tagCount, "S": st,
	}))
}

func (a *app) cabinetTag(w http.ResponseWriter, r *http.Request, s *session) {
	v, err := a.loadTag(r.PathValue("id"), s.BusinessID)
	if err != nil {
		a.notFound(w, err)
		return
	}
	st, err := a.stats("tag", v.T.ID, periodDays(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "cabinet_tag", a.data(r, s, v.T.Name, map[string]any{"V": v, "S": st}))
}

func (a *app) changePassword(w http.ResponseWriter, r *http.Request, s *session) {
	var hash string
	a.db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, s.UserID).Scan(&hash)
	if !checkPassword(hash, r.PostFormValue("old")) {
		a.back(w, r, "/cabinet/", "oldpw")
		return
	}
	if len(r.PostFormValue("new")) < 8 {
		a.back(w, r, "/cabinet/", "pw")
		return
	}
	if _, err := a.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hashPassword(r.PostFormValue("new")), s.UserID); err != nil {
		a.fail(w, err)
		return
	}
	// остальные сессии этого пользователя (например, на чужом устройстве) закрываем
	a.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token <> ?`, s.UserID, s.Token)
	a.done(w, r, "/cabinet/", "pw")
}
