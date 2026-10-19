package main

import (
	"net/http"
	"strings"
)

// Кабинет клиента: видит только своё предприятие, может менять ссылки и вкл/выкл метки,
// смотреть статистику, оплачивать продление и привязать Telegram. Создаёт метки только админ.

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
	pays, err := a.payments(b.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	st, err := a.stats("business", b.ID, periodDays(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "cabinet", a.data(r, s, b.Name, map[string]any{
		"B": b, "Locations": locs, "Payments": pays, "S": st, "PayEnabled": a.yk.enabled(),
	}))
}

func (a *app) cabinetTag(w http.ResponseWriter, r *http.Request, s *session) {
	v, err := a.loadTag(r.PathValue("id"), s.BusinessID)
	if err != nil {
		a.notFound(w, err)
		return
	}
	hist, err := a.history(v.T.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	st, err := a.stats("tag", v.T.ID, periodDays(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "cabinet_tag", a.data(r, s, v.T.Name, map[string]any{"V": v, "NFCURL": a.public + "/t/" + v.T.Token, "History": hist, "S": st}))
}

func (a *app) cabinetUpdateTag(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	back := "/cabinet/g/" + id
	dst := strings.TrimSpace(r.PostFormValue("url"))
	if !validURL(dst) {
		a.back(w, r, back, "url")
		return
	}
	if err := a.saveTag(id, s.BusinessID, s.UserID, "", dst, r.PostFormValue("active") == "1"); err != nil {
		a.notFound(w, err)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
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
	if c, err := r.Cookie("sid"); err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token <> ?`, s.UserID, c.Value)
	}
	a.done(w, r, "/cabinet/", "pw")
}
