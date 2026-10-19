package main

import (
	"fmt"
	"html/template"
	"net/http"
	"time"
)

// Аналитика переходов: по дням, часам, дням недели, устройствам, топ меток.
// Время в БД — unix UTC, группировка по московскому времени (UTC+3, без перехода на летнее).

const mskOffset = 3 * 3600

type bar struct {
	Label string
	Value int
	Style template.CSS // высота/ширина столбика
}

type topTag struct {
	ID             int64
	Name, Location string
	Clicks         int
}

type stats struct {
	Days                            int
	Period, Unique, Today, AllTime  int
	Last                            int64
	Daily, Hours, Weekdays, Devices []bar
	Top                             []topTag
}

func periodDays(r *http.Request) int {
	switch r.URL.Query().Get("days") {
	case "7":
		return 7
	case "90":
		return 90
	}
	return 30
}

// stats: scope "tag" или "business", id — метки или предприятия.
func (a *app) stats(scope string, id int64, days int) (stats, error) {
	where := "c.tag_id = ?"
	if scope == "business" {
		where = "c.tag_id IN (SELECT t.id FROM tags t JOIN locations l ON l.id = t.location_id WHERE l.business_id = ?)"
	}
	now := a.now().In(msk)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, msk)
	from := today.AddDate(0, 0, -(days - 1))
	st := stats{Days: days}

	err := a.db.QueryRow(`SELECT COUNT(*), COUNT(CASE WHEN c.created_at >= ? THEN 1 END), COUNT(CASE WHEN c.created_at >= ? THEN 1 END),
		COUNT(DISTINCT CASE WHEN c.created_at >= ? AND c.visitor <> '' THEN c.visitor END), COALESCE(MAX(c.created_at), 0)
		FROM tag_clicks c WHERE `+where, from.Unix(), today.Unix(), from.Unix(), id).Scan(&st.AllTime, &st.Period, &st.Today, &st.Unique, &st.Last)
	if err != nil {
		return st, err
	}

	group := func(expr string) (map[string]int, error) {
		rows, err := a.db.Query(`SELECT `+expr+`, COUNT(*) FROM tag_clicks c WHERE c.created_at >= ? AND `+where+` GROUP BY 1`, from.Unix(), id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		m := map[string]int{}
		for rows.Next() {
			var k string
			var n int
			rows.Scan(&k, &n)
			m[k] = n
		}
		return m, rows.Err()
	}

	daily, err := group(fmt.Sprintf("date(c.created_at + %d, 'unixepoch')", mskOffset))
	if err != nil {
		return st, err
	}
	for d := from; !d.After(today); d = d.AddDate(0, 0, 1) {
		st.Daily = append(st.Daily, bar{Label: d.Format("02.01"), Value: daily[d.Format(time.DateOnly)]})
	}

	hours, err := group(fmt.Sprintf("strftime('%%H', c.created_at + %d, 'unixepoch')", mskOffset))
	if err != nil {
		return st, err
	}
	for h := range 24 {
		st.Hours = append(st.Hours, bar{Label: fmt.Sprintf("%02d", h), Value: hours[fmt.Sprintf("%02d", h)]})
	}

	wd, err := group(fmt.Sprintf("strftime('%%w', c.created_at + %d, 'unixepoch')", mskOffset)) // 0 = воскресенье
	if err != nil {
		return st, err
	}
	for i, name := range []string{"Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Вс"} {
		st.Weekdays = append(st.Weekdays, bar{Label: name, Value: wd[fmt.Sprint((i+1)%7)]})
	}

	dev, err := group("c.device")
	if err != nil {
		return st, err
	}
	st.Devices = []bar{
		{Label: "iPhone / iPad", Value: dev["ios"]},
		{Label: "Android", Value: dev["android"]},
		{Label: "Другие", Value: st.Period - dev["ios"] - dev["android"]},
	}

	scaleHeight(st.Daily)
	scaleHeight(st.Hours)
	scaleHeight(st.Weekdays)
	for i := range st.Devices {
		pct := 0
		if st.Period > 0 {
			pct = st.Devices[i].Value * 100 / st.Period
		}
		st.Devices[i].Style = template.CSS(fmt.Sprintf("width:%d%%", pct))
	}

	if scope == "business" {
		rows, err := a.db.Query(`SELECT t.id, t.name, l.name, COUNT(*) FROM tag_clicks c JOIN tags t ON t.id = c.tag_id JOIN locations l ON l.id = t.location_id
			WHERE c.created_at >= ? AND l.business_id = ? GROUP BY t.id ORDER BY 4 DESC, t.id LIMIT 10`, from.Unix(), id)
		if err != nil {
			return st, err
		}
		defer rows.Close()
		for rows.Next() {
			var t topTag
			rows.Scan(&t.ID, &t.Name, &t.Location, &t.Clicks)
			st.Top = append(st.Top, t)
		}
	}
	return st, nil
}

func scaleHeight(bars []bar) {
	top := 0
	for _, b := range bars {
		top = max(top, b.Value)
	}
	for i := range bars {
		h := 0
		if top > 0 && bars[i].Value > 0 {
			h = max(bars[i].Value*100/top, 3) // ненулевой день всегда виден
		}
		bars[i].Style = template.CSS(fmt.Sprintf("height:%d%%", h))
	}
}
