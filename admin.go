package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type clientUser struct {
	ID       int64
	Email    string
	TGLinked bool
}

type payment struct {
	ID        int64
	Provider  string
	Amount    int
	Status    string
	CreatedAt int64
	PaidAt    sql.NullInt64
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request) {
	s := a.session(r)
	if s == nil {
		var n int
		a.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
		if n == 0 {
			a.authPage(w, 200, true, a.panel+"/setup", "")
		} else {
			a.authPage(w, 200, false, a.panel+"/login", "")
		}
		return
	}
	if s.Role != "admin" {
		http.Redirect(w, r, a.home(s.Role), http.StatusSeeOther)
		return
	}
	since := a.now().AddDate(0, 0, -30).Unix()
	rows, err := a.db.Query(`SELECT b.id, b.name, b.plan, b.starts_at, b.expires_at, b.price,
		(SELECT COUNT(*) FROM locations l WHERE l.business_id = b.id),
		(SELECT COUNT(*) FROM tags t JOIN locations l ON l.id = t.location_id WHERE l.business_id = b.id),
		(SELECT COUNT(*) FROM tag_clicks c JOIN tags t ON t.id = c.tag_id JOIN locations l ON l.id = t.location_id WHERE l.business_id = b.id AND c.created_at >= ?)
		FROM businesses b ORDER BY b.expires_at, b.name`, since)
	if err != nil {
		a.fail(w, err)
		return
	}
	defer rows.Close()
	var list []business
	for rows.Next() {
		var b business
		rows.Scan(&b.ID, &b.Name, &b.Plan, &b.StartsAt, &b.ExpiresAt, &b.Price, &b.Locations, &b.Tags, &b.Taps30)
		b.Status = subStatus(b.ExpiresAt, a.now())
		list = append(list, b)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, err)
		return
	}
	attention := 0
	for _, b := range list {
		if b.Status.Class != "badge-ok" {
			attention++
		}
	}
	var activeTags int
	a.db.QueryRow(`SELECT COUNT(*) FROM tags WHERE active = 1`).Scan(&activeTags)
	st, err := a.stats("all", 0, periodDays(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "dashboard", a.data(r, s, "Обзор", map[string]any{
		"Businesses": list, "S": st, "ActiveTags": activeTags, "Attention": attention,
		"DefaultExp": a.now().In(msk).AddDate(1, 0, 0).Format(time.DateOnly),
	}))
}

// --- предприятия ---

func (a *app) createBusiness(w http.ResponseWriter, r *http.Request, s *session) {
	name, exp := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("expires_at")
	price, err := strconv.Atoi(r.PostFormValue("price"))
	switch {
	case name == "":
		a.back(w, r, a.panel+"/", "empty")
	case !validDate(exp):
		a.back(w, r, a.panel+"/", "date")
	case err != nil || price <= 0:
		a.back(w, r, a.panel+"/", "amount")
	default:
		today := a.now().In(msk).Format(time.DateOnly)
		res, err := a.db.Exec(`INSERT INTO businesses(name, plan, starts_at, expires_at, price) VALUES(?, ?, ?, ?, ?)`, name, strings.TrimSpace(r.PostFormValue("plan")), today, exp, price)
		if err != nil {
			a.fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		http.Redirect(w, r, fmt.Sprintf("%s/b/%d", a.panel, id), http.StatusSeeOther)
	}
}

func (a *app) businessPage(w http.ResponseWriter, r *http.Request, s *session) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b, err := a.business(id)
	if err != nil {
		a.notFound(w, err)
		return
	}
	locs, err := a.locations(b.ID, false)
	if err != nil {
		a.fail(w, err)
		return
	}
	clients, err := a.clients(b.ID)
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
	a.render(w, 200, "business", a.data(r, s, b.Name, map[string]any{"B": b, "Locations": locs, "Clients": clients, "Payments": pays, "S": st}))
}

func (a *app) updateBusiness(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	back := a.panel + "/b/" + id
	name, start, exp := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("starts_at"), r.PostFormValue("expires_at")
	price, err := strconv.Atoi(r.PostFormValue("price"))
	switch {
	case name == "":
		a.back(w, r, back, "empty")
	case !validDate(start) || !validDate(exp):
		a.back(w, r, back, "date")
	case err != nil || price <= 0:
		a.back(w, r, back, "amount")
	default:
		if _, err := a.db.Exec(`UPDATE businesses SET name = ?, plan = ?, starts_at = ?, expires_at = ?, price = ? WHERE id = ?`, name, strings.TrimSpace(r.PostFormValue("plan")), start, exp, price, id); err != nil {
			a.fail(w, err)
			return
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

// manualPayment — оплата наличными/переводом: та же запись платежа и продление, что и через ЮKassa.
func (a *app) manualPayment(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	back := a.panel + "/b/" + id
	amount, err := strconv.Atoi(r.PostFormValue("amount"))
	if err != nil || amount <= 0 {
		a.back(w, r, back, "amount")
		return
	}
	res, err := a.db.Exec(`INSERT INTO payments(business_id, user_id, provider, amount, status, created_at) SELECT id, ?, 'manual', ?, 'pending', ? FROM businesses WHERE id = ?`, s.UserID, amount, a.now().Unix(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.message(w, 404, "Не найдено")
		return
	}
	pid, _ := res.LastInsertId()
	if err := a.markPaid(pid, false); err != nil {
		a.fail(w, err)
		return
	}
	a.done(w, r, back, "paid")
}

// --- доступ клиентов ---

func (a *app) createClient(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	back := a.panel + "/b/" + id
	email, pw := strings.TrimSpace(r.PostFormValue("email")), r.PostFormValue("password")
	if !validEmail(email) || len(pw) < 8 {
		a.back(w, r, back, "pw")
		return
	}
	_, err := a.db.Exec(`INSERT INTO users(email, password_hash, role, business_id) SELECT ?, ?, 'client', id FROM businesses WHERE id = ?`, email, hashPassword(pw), id)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		a.back(w, r, back, "exists")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	a.done(w, r, back, "client")
}

func (a *app) deleteClient(w http.ResponseWriter, r *http.Request, s *session) {
	var biz int64
	if err := a.db.QueryRow(`SELECT business_id FROM users WHERE id = ? AND role = 'client'`, r.PathValue("id")).Scan(&biz); err != nil {
		a.notFound(w, err)
		return
	}
	if _, err := a.db.Exec(`DELETE FROM users WHERE id = ? AND role = 'client'`, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/b/%d", a.panel, biz), http.StatusSeeOther)
}

// --- точки ---

func (a *app) createLocation(w http.ResponseWriter, r *http.Request, s *session) {
	bid := r.PathValue("id")
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		a.back(w, r, a.panel+"/b/"+bid, "empty")
		return
	}
	res, err := a.db.Exec(`INSERT INTO locations(business_id, name, address) SELECT id, ?, ? FROM businesses WHERE id = ?`, name, strings.TrimSpace(r.PostFormValue("address")), bid)
	if err != nil {
		a.fail(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.message(w, 404, "Не найдено")
		return
	}
	id, _ := res.LastInsertId()
	http.Redirect(w, r, fmt.Sprintf("%s/l/%d", a.panel, id), http.StatusSeeOther)
}

func (a *app) locationPage(w http.ResponseWriter, r *http.Request, s *session) {
	var l location
	var bname string
	err := a.db.QueryRow(`SELECT l.id, l.business_id, l.name, l.address, b.name FROM locations l JOIN businesses b ON b.id = l.business_id WHERE l.id = ?`, r.PathValue("id")).
		Scan(&l.ID, &l.BusinessID, &l.Name, &l.Address, &bname)
	if err != nil {
		a.notFound(w, err)
		return
	}
	tags, err := a.tagsOf(l.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.render(w, 200, "location", a.data(r, s, l.Name, map[string]any{"L": l, "BusinessName": bname, "Tags": tags}))
}

// --- метки ---

func (a *app) createTag(w http.ResponseWriter, r *http.Request, s *session) {
	lid := r.PathValue("id")
	back := a.panel + "/l/" + lid
	name, dst := strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("url"))
	if name == "" {
		a.back(w, r, back, "empty")
		return
	}
	if !validURL(dst) {
		a.back(w, r, back, "url")
		return
	}
	var res sql.Result
	var err error
	for range 5 { // токен уникален в БД; при редком совпадении пробуем другой
		res, err = a.db.Exec(`INSERT INTO tags(location_id, public_token, name, destination_url, created_at) SELECT id, ?, ?, ?, ? FROM locations WHERE id = ?`,
			newToken(), name, dst, a.now().Unix(), lid)
		if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
			break
		}
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.message(w, 404, "Не найдено")
		return
	}
	id, _ := res.LastInsertId()
	http.Redirect(w, r, fmt.Sprintf("%s/g/%d", a.panel, id), http.StatusSeeOther)
}

func (a *app) tagPage(w http.ResponseWriter, r *http.Request, s *session) {
	v, err := a.loadTag(r.PathValue("id"), 0)
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
	a.render(w, 200, "tag", a.data(r, s, v.T.Name, map[string]any{"V": v, "NFCURL": a.public + "/t/" + v.T.Token, "History": hist, "S": st}))
}

func (a *app) updateTag(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	back := a.panel + "/g/" + id
	name, dst := strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("url"))
	if name == "" {
		a.back(w, r, back, "empty")
		return
	}
	if !validURL(dst) {
		a.back(w, r, back, "url")
		return
	}
	if err := a.saveTag(id, s.UserID, name, dst, r.PostFormValue("active") == "1"); err != nil {
		a.notFound(w, err)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// --- выборки ---

// locations: withTags — подгрузить метки каждой точки (для кабинета).
func (a *app) locations(biz int64, withTags bool) ([]location, error) {
	rows, err := a.db.Query(`SELECT l.id, l.name, l.address, (SELECT COUNT(*) FROM tags t WHERE t.location_id = l.id) FROM locations l WHERE l.business_id = ? ORDER BY l.name`, biz)
	if err != nil {
		return nil, err
	}
	var locs []location
	for rows.Next() {
		var l location
		rows.Scan(&l.ID, &l.Name, &l.Address, &l.Tags)
		locs = append(locs, l)
	}
	rows.Close()
	if withTags {
		for i := range locs {
			if locs[i].TagList, err = a.tagsOf(locs[i].ID); err != nil {
				return nil, err
			}
		}
	}
	return locs, rows.Err()
}

func (a *app) tagsOf(loc int64) ([]tag, error) {
	rows, err := a.db.Query(`SELECT t.id, t.name, t.active, (SELECT COUNT(*) FROM tag_clicks c WHERE c.tag_id = t.id) FROM tags t WHERE t.location_id = ? ORDER BY t.id`, loc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []tag
	for rows.Next() {
		var t tag
		rows.Scan(&t.ID, &t.Name, &t.Active, &t.Clicks)
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

func (a *app) clients(biz int64) ([]clientUser, error) {
	rows, err := a.db.Query(`SELECT id, email, tg_chat_id IS NOT NULL FROM users WHERE business_id = ? AND role = 'client' ORDER BY email`, biz)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []clientUser
	for rows.Next() {
		var c clientUser
		rows.Scan(&c.ID, &c.Email, &c.TGLinked)
		list = append(list, c)
	}
	return list, rows.Err()
}

func (a *app) payments(biz int64) ([]payment, error) {
	rows, err := a.db.Query(`SELECT id, provider, amount, status, created_at, paid_at FROM payments WHERE business_id = ? AND status <> 'error' ORDER BY id DESC LIMIT 20`, biz)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []payment
	for rows.Next() {
		var p payment
		rows.Scan(&p.ID, &p.Provider, &p.Amount, &p.Status, &p.CreatedAt, &p.PaidAt)
		list = append(list, p)
	}
	return list, rows.Err()
}
